import Foundation
import NetworkExtension
import Mobilecore

final class PacketFlowBridge: NSObject, MobilecorePacketIOProtocol, @unchecked Sendable {
    private let packetFlow: NEPacketTunnelFlow
    private let condition = NSCondition()
    private let writeQueue = DispatchQueue(label: "io.github.bojieli.queqiao.packet-write")
    // A fixed descriptor ring avoids Array.removeFirst() copying and, more
    // importantly, prevents traffic bursts from growing retained Data objects.
    // The byte watermark is the binding capacity; the count also bounds a
    // stream of tiny packets.
    private static let queueLimit = 64
    private static let queueByteLimit = 128 * 1_024
    private static let refillPacketWatermark = 16
    private static let refillByteWatermark = 32 * 1_024
    private var packets = [Data?](repeating: nil, count: PacketFlowBridge.queueLimit)
    private var head = 0
    private var count = 0
    private var queuedBytes = 0
    private var readInFlight = false
    private var started = false
    private var closed = false

    init(packetFlow: NEPacketTunnelFlow) {
        self.packetFlow = packetFlow
        super.init()
    }

    func start() {
        condition.lock()
        guard !started, !closed else {
            condition.unlock()
            return
        }
        started = true
        condition.unlock()
        requestRead(force: true)
    }

    func readPacket() -> Data? {
        condition.lock()
        while count == 0 && !closed {
            condition.wait()
        }
        guard count > 0, let packet = packets[head] else {
            condition.unlock()
            return Data()
        }
        packets[head] = nil
        head = (head + 1) % Self.queueLimit
        count -= 1
        queuedBytes -= packet.count
        let shouldRefill = count < Self.refillPacketWatermark
            && queuedBytes < Self.refillByteWatermark
        condition.unlock()
        if shouldRefill {
            requestRead(force: false)
        }
        return packet
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
            autoreleasepool {
                packetFlow.writePackets([packet], withProtocols: [NSNumber(value: family)])
            }
        }
    }

    func close() {
        condition.lock()
        closed = true
        for index in packets.indices {
            packets[index] = nil
        }
        head = 0
        count = 0
        queuedBytes = 0
        condition.broadcast()
        condition.unlock()
    }

    private func requestRead(force: Bool) {
        condition.lock()
        let belowWatermark = count < Self.refillPacketWatermark
            && queuedBytes < Self.refillByteWatermark
        let shouldRead = started && !closed && !readInFlight && (force || belowWatermark)
        if shouldRead {
            readInFlight = true
        }
        condition.unlock()
        guard shouldRead else { return }

        packetFlow.readPackets { [weak self] packets, _ in
            guard let self else { return }
            condition.lock()
            readInFlight = false
            if !closed {
                for packet in packets where !packet.isEmpty {
                    guard count < Self.queueLimit,
                          packet.count <= Self.queueByteLimit - queuedBytes else {
                        continue
                    }
                    let tail = (head + count) % Self.queueLimit
                    self.packets[tail] = packet
                    count += 1
                    queuedBytes += packet.count
                }
                condition.broadcast()
            }
            // If Apple returned only unusable/oversized packets, keep one read
            // outstanding so the Go reader cannot remain asleep indefinitely.
            let shouldContinue = !closed && count == 0
            condition.unlock()
            if shouldContinue {
                requestRead(force: true)
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
