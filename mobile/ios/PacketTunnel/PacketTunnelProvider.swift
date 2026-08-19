import Foundation
import NetworkExtension
import OSLog
import Mobilecore

final class PacketTunnelProvider: NEPacketTunnelProvider, MobilecoreObserverProtocol, @unchecked Sendable {
    private let logger = Logger(subsystem: "io.github.bojieli.queqiao", category: "packet-tunnel")
    private let engineQueue = DispatchQueue(
        label: "io.github.bojieli.queqiao.packet-engine",
        qos: .userInitiated
    )
    private let lifecycle = PacketTunnelLifecycle()

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping (Error?) -> Void
    ) {
        let completion = OneShotErrorCompletion(completionHandler)
        let startup = lifecycle.beginStartup(completion: completion)
        do {
            guard let configuration = protocolConfiguration as? NETunnelProviderProtocol,
                  let profileID = configuration.providerConfiguration?["profileID"] as? String,
                  !profileID.isEmpty else {
                throw TunnelError.missingProfileSelection
            }
            let store = try ProfileStore()
            guard let (record, profile) = try store.profile(id: profileID) else {
                throw TunnelError.missingProfile
            }
            try MobileCore.validateProfile(profile)
            let requestedPolicy = configuration.providerConfiguration?["trafficPolicy"] as? String
            let policy = TrafficPolicy(rawValue: requestedPolicy ?? "") ?? record.trafficPolicy
            guard lifecycle.selectProfile(profileID, for: startup) else {
                throw TunnelError.startCancelled
            }
            recordDiagnostic(
                level: .info,
                "Starting profile \(record.displayName) with \(policy.title.lowercased())"
            )
            resolveAndConfigureTunnel(
                endpoint: record.summary.endpoint,
                profile: profile,
                routing: TunnelRouting(policy: policy, bypassRoutes: record.bypassRoutes),
                startup: startup,
                completion: completion
            )
        } catch {
            recordDiagnostic(level: .error, "Tunnel startup failed: \(error.localizedDescription)")
            completion.call(error)
            lifecycle.invalidate(startup)
        }
    }

    override func stopTunnel(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        let completion = OneShotVoidCompletion(completionHandler)
        let resources = lifecycle.beginStop()
        resources.startCompletion?.call(TunnelError.startCancelled)
        recordDiagnostic(
            level: .info,
            "Tunnel stopped: \(reason.diagnosticName) (iOS reason \(reason.rawValue))"
        )
        engineQueue.async { [self] in
            resources.bridge?.close()
            do {
                try resources.session?.stopChecked()
            } catch {
                let detail = DiagnosticStore.sanitize(error.localizedDescription)
                logger.error("Tunnel stop reported an error: \(detail, privacy: .private)")
            }
            completion.call()
        }
    }

    override func sleep(completionHandler: @escaping () -> Void) {
        recordDiagnostic(level: .info, "Device sleeping; tunnel remains configured")
        completionHandler()
    }

    override func wake() {
        recordDiagnostic(level: .info, "Device woke; tunnel provider resumed")
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)? = nil) {
        let metrics = lifecycle.currentSession?.metricsJSON() ?? "{\"version\":2,\"state\":\"stopped\"}"
        completionHandler?(metrics.data(using: .utf8))
    }

    func onStateChanged(_ state: String?) {
        guard let state else { return }
        logger.info("Tunnel state: \(state, privacy: .public)")
        recordDiagnostic(level: .info, "Packet engine state: \(state)")
        if state == MobilecoreStateFailed && !lifecycle.isStopping {
            recordDiagnostic(level: .error, "Packet engine entered the failed state")
            cancelTunnelWithError(TunnelError.coreStopped)
        }
    }

    func onLog(_ level: String?, message: String?) {
        guard let message else { return }
        let normalizedLevel = level?.uppercased()
        // Core failures can contain provider addresses or credential-shaped
        // values supplied by lower layers. Keep useful text in the encrypted
        // diagnostic ring, but never publish the raw interpolation to OSLog.
        let sanitized = DiagnosticStore.sanitize(message)
        if normalizedLevel == "ERROR" {
            logger.error("\(sanitized, privacy: .private)")
            recordDiagnostic(level: .error, sanitized)
        } else if normalizedLevel == "WARN" || normalizedLevel == "WARNING" {
            logger.warning("\(sanitized, privacy: .private)")
            recordDiagnostic(level: .warning, sanitized)
        } else {
            logger.info("\(sanitized, privacy: .private)")
        }
    }

    private func recordDiagnostic(level: DiagnosticLevel, _ message: String) {
        try? DiagnosticStore().append(level: level, component: "Packet tunnel", message: message)
    }

    private func configureTunnel(
        profile: String,
        routing: TunnelRouting,
        remoteAddress: String,
        startup: StartupAttempt,
        completion: OneShotErrorCompletion
    ) {
        let plan = routePlan(for: routing)
        recordDiagnostic(level: .info, "Route plan: \(plan.diagnosticSummary)")
        setTunnelNetworkSettings(
            makeNetworkSettings(plan: plan, remoteAddress: remoteAddress)
        ) { [self] error in
            if let error {
                recordDiagnostic(
                    level: .error,
                    "Could not configure the iOS tunnel: \(error.localizedDescription)"
                )
                completion.call(error)
                lifecycle.invalidate(startup)
                return
            }
            // Returning from Apple's settings callback promptly is important:
            // cold initialization of the statically linked Go/gVisor runtime
            // must not block NetworkExtension's internal callback queue.
            engineQueue.async { [self] in
                startPacketEngine(profile: profile, startup: startup, completion: completion)
            }
        }
    }

    private func resolveAndConfigureTunnel(
        endpoint: String,
        profile: String,
        routing: TunnelRouting,
        startup: StartupAttempt,
        completion: OneShotErrorCompletion
    ) {
        engineQueue.async { [self] in
            do {
                guard lifecycle.isActive(startup) else {
                    completion.call(TunnelError.startCancelled)
                    return
                }
                let remoteAddress = try ProviderEndpoint.resolvedAddress(from: endpoint)
                recordDiagnostic(level: .info, "Provider endpoint resolved to \(remoteAddress)")
                configureTunnel(
                    profile: profile,
                    routing: routing,
                    remoteAddress: remoteAddress,
                    startup: startup,
                    completion: completion
                )
            } catch {
                recordDiagnostic(level: .error, "Provider resolution failed: \(error.localizedDescription)")
                completion.call(error)
                lifecycle.invalidate(startup)
            }
        }
    }

    private func startPacketEngine(
        profile: String,
        startup: StartupAttempt,
        completion: OneShotErrorCompletion
    ) {
        guard lifecycle.isActive(startup) else {
            completion.call(TunnelError.startCancelled)
            return
        }
        recordDiagnostic(level: .info, "iOS tunnel interface configured; initializing packet engine")
        let packetBridge = PacketFlowBridge(packetFlow: packetFlow)
        do {
            guard let newSession = MobilecoreNewSession(self, nil) else {
                throw TunnelError.coreStopped
            }
            packetBridge.start()
            try newSession.startChecked(profile: profile, packetIO: packetBridge, mtu: 1_280)
            guard lifecycle.install(
                session: newSession,
                bridge: packetBridge,
                for: startup
            ) else {
                packetBridge.close()
                try? newSession.stopChecked()
                completion.call(TunnelError.startCancelled)
                return
            }
            if completion.call(nil) {
                lifecycle.finish(startup)
                recordDiagnostic(
                    level: .info,
                    "Tunnel ready in \(startup.elapsedMilliseconds) ms"
                )
            }
        } catch {
            packetBridge.close()
            recordDiagnostic(level: .error, "Packet engine startup failed: \(error.localizedDescription)")
            completion.call(error)
            lifecycle.invalidate(startup)
        }
    }

    func onProfileUpdated(_ profileJSON: String?) -> Bool {
        let profileID = lifecycle.activeProfileID
        guard let profileJSON, let profileID else { return false }
        do {
            try MobileCore.validateProfile(profileJSON)
            try ProfileStore().replaceProfile(profileJSON, id: profileID)
            return true
        } catch {
            logger.error("Could not persist renewed device identity: \(error.localizedDescription, privacy: .public)")
            return false
        }
    }

    /// The set of destinations that stay off the tunnel for this profile.
    ///
    /// Everything about how those prefixes are parsed, deduplicated, coalesced
    /// and capped lives in RoutePlan so it can be tested without a
    /// NetworkExtension host.
    private func routePlan(for routing: TunnelRouting) -> RoutePlan {
        switch routing.policy {
        case .allTraffic:
            return RoutePlan.make(userRoutes: routing.bypassRoutes)
        case .excludeLocalNetworks:
            return RoutePlan.make(userRoutes: RoutePlan.localNetworks + routing.bypassRoutes)
        }
    }

    private func makeNetworkSettings(
        plan: RoutePlan,
        remoteAddress: String
    ) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: remoteAddress)
        settings.mtu = 1_280

        let ipv4 = NEIPv4Settings(addresses: ["10.77.0.2"], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = [.default()]
        let ipv4Excluded = plan.ipv4Routes
        if !ipv4Excluded.isEmpty {
            ipv4.excludedRoutes = ipv4Excluded
        }
        settings.ipv4Settings = ipv4

        let ipv6 = NEIPv6Settings(addresses: ["fd77:7171:6f::2"], networkPrefixLengths: [128])
        ipv6.includedRoutes = [.default()]
        let ipv6Excluded = plan.ipv6Routes
        if !ipv6Excluded.isEmpty {
            ipv6.excludedRoutes = ipv6Excluded
        }
        settings.ipv6Settings = ipv6

        let dns = NEDNSSettings(servers: ["1.1.1.1", "2606:4700:4700::1111"])
        dns.matchDomains = [""]
        settings.dnsSettings = dns
        return settings
    }
}

/// Where this profile's traffic goes, read once from the stored record at
/// startup and carried through resolution into the settings build.
private struct TunnelRouting {
    let policy: TrafficPolicy
    let bypassRoutes: [String]
}

private enum TunnelError: LocalizedError {
    case missingProfile
    case missingProfileSelection
    case coreStopped
    case startCancelled

    var errorDescription: String? {
        switch self {
        case .missingProfile:
            return "No enrolled Queqiao device identity is available."
        case .missingProfileSelection:
            return "The VPN configuration does not identify a Queqiao profile. Open the app and connect again."
        case .coreStopped:
            return "The Queqiao packet engine stopped unexpectedly."
        case .startCancelled:
            return "Tunnel startup was cancelled."
        }
    }
}
