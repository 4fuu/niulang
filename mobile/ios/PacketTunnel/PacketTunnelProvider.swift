import Foundation
import NetworkExtension
import OSLog
import Mobilecore

final class PacketTunnelProvider: NEPacketTunnelProvider, MobilecoreObserverProtocol, @unchecked Sendable {
    private let logger = Logger(subsystem: "io.github.bojieli.queqiao", category: "packet-tunnel")
    private let stateLock = NSLock()
    private var session: MobilecoreSession?
    private var bridge: PacketFlowBridge?
    private var stopping = false
    private var activeProfileID: String?

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping (Error?) -> Void
    ) {
        stateLock.lock()
        stopping = false
        stateLock.unlock()
        let completion = StartCompletion(completionHandler)
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
            activeProfileID = profileID
            recordDiagnostic(
                level: .info,
                "Starting profile \(record.displayName) with \(policy.title.lowercased())"
            )
            resolveAndConfigureTunnel(
                endpoint: record.summary.endpoint,
                profile: profile,
                policy: policy,
                completion: completion
            )
        } catch {
            recordDiagnostic(level: .error, "Tunnel startup failed: \(error.localizedDescription)")
            completion.call(error)
        }
    }

    override func stopTunnel(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        stateLock.lock()
        stopping = true
        stateLock.unlock()
        recordDiagnostic(level: .info, "Tunnel stopped (iOS reason \(reason.rawValue))")
        bridge?.close()
        do {
            try session?.stopChecked()
        } catch {
            logger.error("Tunnel stop reported an error: \(error.localizedDescription, privacy: .public)")
        }
        session = nil
        bridge = nil
        activeProfileID = nil
        completionHandler()
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)? = nil) {
        let metrics = session?.metricsJSON() ?? "{\"version\":1,\"state\":\"stopped\"}"
        completionHandler?(metrics.data(using: .utf8))
    }

    func onStateChanged(_ state: String?) {
        guard let state else { return }
        logger.info("Tunnel state: \(state, privacy: .public)")
        stateLock.lock()
        let isStopping = stopping
        stateLock.unlock()
        if state == MobilecoreStateFailed && !isStopping {
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
        completion: StartCompletion
    ) {
        setTunnelNetworkSettings(
            makeNetworkSettings(policy: policy, remoteAddress: remoteAddress)
        ) { [weak self] error in
            guard let self else { return }
            if let error {
                recordDiagnostic(
                    level: .error,
                    "Could not configure the iOS tunnel: \(error.localizedDescription)"
                )
                completion.call(error)
                return
            }
            recordDiagnostic(level: .info, "iOS tunnel interface configured")
            do {
                let packetBridge = PacketFlowBridge(packetFlow: packetFlow)
                guard let newSession = MobilecoreNewSession(self, nil) else {
                    throw TunnelError.coreStopped
                }
                bridge = packetBridge
                session = newSession
                packetBridge.start()
                try newSession.startChecked(profile: profile, packetIO: packetBridge, mtu: 1_280)
                completion.call(nil)
            } catch {
                recordDiagnostic(level: .error, "Packet engine startup failed: \(error.localizedDescription)")
                bridge?.close()
                bridge = nil
                session = nil
                completion.call(error)
            }
        }
    }

    private func resolveAndConfigureTunnel(
        endpoint: String,
        profile: String,
        policy: TrafficPolicy,
        completion: StartCompletion
    ) {
        DispatchQueue.global(qos: .userInitiated).async { [self] in
            do {
                let remoteAddress = try ProviderEndpoint.resolvedAddress(from: endpoint)
                recordDiagnostic(level: .info, "Provider endpoint resolved to \(remoteAddress)")
                configureTunnel(
                    profile: profile,
                    policy: policy,
                    remoteAddress: remoteAddress,
                    completion: completion
                )
            } catch {
                recordDiagnostic(level: .error, "Provider resolution failed: \(error.localizedDescription)")
                completion.call(error)
            }
        }
    }

    func onProfileUpdated(_ profileJSON: String?) -> Bool {
        guard let profileJSON, let activeProfileID else { return false }
        do {
            try MobileCore.validateProfile(profileJSON)
            try ProfileStore().replaceProfile(profileJSON, id: activeProfileID)
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

private final class StartCompletion: @unchecked Sendable {
    private let handler: (Error?) -> Void

    init(_ handler: @escaping (Error?) -> Void) {
        self.handler = handler
    }

    func call(_ error: Error?) {
        handler(error)
    }
}

private enum TunnelError: LocalizedError {
    case missingProfile
    case missingProfileSelection
    case coreStopped

    var errorDescription: String? {
        switch self {
        case .missingProfile:
            return "No enrolled Queqiao device identity is available."
        case .missingProfileSelection:
            return "The VPN configuration does not identify a Queqiao profile. Open the app and connect again."
        case .coreStopped:
            return "The Queqiao packet engine stopped unexpectedly."
        }
    }
}
