import Foundation
import NetworkExtension
import Mobilecore

final class PacketFlowBridge: NSObject, MobilecorePacketIOProtocol, @unchecked Sendable {
    private let packetFlow: NEPacketTunnelFlow
    private let condition = NSCondition()
    private let writeQueue = DispatchQueue(label: "io.github.bojieli.queqiao.packet-write")
    private var queuedPackets: [Data] = []
    private var closed = false
    private let queueLimit = 1_024

    init(packetFlow: NEPacketTunnelFlow) {
        self.packetFlow = packetFlow
        super.init()
    }

    func start() {
        scheduleRead()
    }

    func readPacket() -> Data? {
        condition.lock()
        defer { condition.unlock() }
        while queuedPackets.isEmpty && !closed {
            condition.wait()
        }
        guard !queuedPackets.isEmpty else {
            return Data()
        }
        return queuedPackets.removeFirst()
    }

    func writePacket(_ packet: Data?) -> Bool {
        guard let packet, !packet.isEmpty, let family = Self.family(for: packet) else {
            return false
        }
        condition.lock()
        let isClosed = closed
        condition.unlock()
        guard !isClosed else {
            return false
        }
        return writeQueue.sync {
            packetFlow.writePackets([packet], withProtocols: [NSNumber(value: family)])
        }
    }

    func close() {
        condition.lock()
        closed = true
        queuedPackets.removeAll(keepingCapacity: false)
        condition.broadcast()
        condition.unlock()
    }

    private func scheduleRead() {
        packetFlow.readPackets { [weak self] packets, _ in
            guard let self else { return }
            condition.lock()
            if !closed {
                let room = max(0, queueLimit - queuedPackets.count)
                queuedPackets.append(contentsOf: packets.prefix(room))
                condition.broadcast()
            }
            let shouldContinue = !closed
            condition.unlock()
            if shouldContinue {
                scheduleRead()
            }
        }
    }

    private static func family(for packet: Data) -> Int32? {
        guard let first = packet.first else { return nil }
        switch first >> 4 {
        case 4: return AF_INET
        case 6: return AF_INET6
        default: return nil
        }
    }
}
