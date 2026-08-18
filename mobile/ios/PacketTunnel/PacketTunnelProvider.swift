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
                policy: policy,
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
        recordDiagnostic(level: .info, "Tunnel stopped (iOS reason \(reason.rawValue))")
        engineQueue.async { [self] in
            resources.bridge?.close()
            do {
                try resources.session?.stopChecked()
            } catch {
                logger.error("Tunnel stop reported an error: \(error.localizedDescription, privacy: .public)")
            }
            completion.call()
        }
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)? = nil) {
        let metrics = lifecycle.currentSession?.metricsJSON() ?? "{\"version\":1,\"state\":\"stopped\"}"
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
        if normalizedLevel == "ERROR" {
            logger.error("\(message, privacy: .public)")
            recordDiagnostic(level: .error, message)
        } else if normalizedLevel == "WARN" || normalizedLevel == "WARNING" {
            logger.warning("\(message, privacy: .public)")
            recordDiagnostic(level: .warning, message)
        } else {
            logger.info("\(message, privacy: .public)")
        }
    }

    private func recordDiagnostic(level: DiagnosticLevel, _ message: String) {
        try? DiagnosticStore().append(level: level, component: "Packet tunnel", message: message)
    }

    private func configureTunnel(
        profile: String,
        policy: TrafficPolicy,
        remoteAddress: String,
        startup: StartupAttempt,
        completion: OneShotErrorCompletion
    ) {
        setTunnelNetworkSettings(
            makeNetworkSettings(policy: policy, remoteAddress: remoteAddress)
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
        policy: TrafficPolicy,
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
                    policy: policy,
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

    private func makeNetworkSettings(
        policy: TrafficPolicy,
        remoteAddress: String
    ) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: remoteAddress)
        settings.mtu = 1_280

        let ipv4 = NEIPv4Settings(addresses: ["10.77.0.2"], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = [.default()]
        if policy == .excludeLocalNetworks {
            ipv4.excludedRoutes = [
                NEIPv4Route(destinationAddress: "10.0.0.0", subnetMask: "255.0.0.0"),
                NEIPv4Route(destinationAddress: "100.64.0.0", subnetMask: "255.192.0.0"),
                NEIPv4Route(destinationAddress: "127.0.0.0", subnetMask: "255.0.0.0"),
                NEIPv4Route(destinationAddress: "169.254.0.0", subnetMask: "255.255.0.0"),
                NEIPv4Route(destinationAddress: "172.16.0.0", subnetMask: "255.240.0.0"),
                NEIPv4Route(destinationAddress: "192.168.0.0", subnetMask: "255.255.0.0")
            ]
        }
        settings.ipv4Settings = ipv4

        let ipv6 = NEIPv6Settings(addresses: ["fd77:7171:6f::2"], networkPrefixLengths: [128])
        ipv6.includedRoutes = [.default()]
        if policy == .excludeLocalNetworks {
            ipv6.excludedRoutes = [
                NEIPv6Route(destinationAddress: "::1", networkPrefixLength: 128),
                NEIPv6Route(destinationAddress: "fc00::", networkPrefixLength: 7),
                NEIPv6Route(destinationAddress: "fe80::", networkPrefixLength: 10)
            ]
        }
        settings.ipv6Settings = ipv6

        let dns = NEDNSSettings(servers: ["1.1.1.1", "2606:4700:4700::1111"])
        dns.matchDomains = [""]
        settings.dnsSettings = dns
        return settings
    }
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
