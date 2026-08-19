import Darwin
import Foundation

/// One CIDR block, normalized so that every host bit is zero.
///
/// The address is held as a 128-bit pair rather than a byte array because a
/// bypass set can carry thousands of prefixes and the packet-tunnel extension
/// runs against a fixed memory profile. Two words inline cost nothing; a
/// per-prefix heap allocation would.
///
/// IPv4 lives in the low 32 bits of `low`, which keeps one comparison, one
/// mask, and one containment test serving both families.
struct IPPrefix: Hashable, Comparable, Sendable {
    enum Family: UInt8, Sendable {
        case ipv4
        case ipv6

        var bitWidth: Int { self == .ipv4 ? 32 : 128 }
    }

    /// An address before a prefix length is applied to it.
    private struct RawAddress {
        let family: Family
        let high: UInt64
        let low: UInt64
    }

    let family: Family
    let high: UInt64
    let low: UInt64
    let length: Int

    private init(family: Family, high: UInt64, low: UInt64, length: Int) {
        self.family = family
        self.length = length
        let masked = IPPrefix.mask(high: high, low: low, length: length, bitWidth: family.bitWidth)
        self.high = masked.high
        self.low = masked.low
    }

    /// Parses "10.0.0.0/8" or "fc00::/7". A bare address is taken as a host
    /// route, because that is what someone typing a single address means.
    init?(cidr: String) {
        let trimmed = cidr.trimmingCharacters(in: .whitespaces)
        guard !trimmed.isEmpty else { return nil }
        let parts = trimmed.split(separator: "/", omittingEmptySubsequences: false)
        guard parts.count <= 2 else { return nil }
        let address = String(parts[0])
        guard let parsed = IPPrefix.parseAddress(address) else { return nil }
        let length: Int
        if parts.count == 2 {
            guard let requested = Int(parts[1]) else { return nil }
            length = requested
        } else {
            length = parsed.family.bitWidth
        }
        guard length >= 0, length <= parsed.family.bitWidth else { return nil }
        self.init(family: parsed.family, high: parsed.high, low: parsed.low, length: length)
    }

    /// Builds a prefix from an address already in memory.
    ///
    /// The bundled country set arrives as packed bytes, and rendering each of
    /// several thousand entries to text only to parse it back would cost more
    /// allocations than the extension's memory profile has to spare.
    init?(ipv4 address: UInt32, length: Int) {
        guard length >= 0, length <= 32 else { return nil }
        self.init(family: .ipv4, high: 0, low: UInt64(address), length: length)
    }

    init?(ipv6 high: UInt64, low: UInt64, length: Int) {
        guard length >= 0, length <= 128 else { return nil }
        self.init(family: .ipv6, high: high, low: low, length: length)
    }

    static func < (lhs: IPPrefix, rhs: IPPrefix) -> Bool {
        if lhs.family != rhs.family { return lhs.family.rawValue < rhs.family.rawValue }
        if lhs.high != rhs.high { return lhs.high < rhs.high }
        if lhs.low != rhs.low { return lhs.low < rhs.low }
        return lhs.length < rhs.length
    }

    /// The number of addresses this block covers, saturating for very large
    /// IPv6 blocks. Used to rank prefixes when a route cap forces a choice.
    var coverage: UInt64 {
        let hostBits = family.bitWidth - length
        return hostBits >= 64 ? UInt64.max : (UInt64(1) << UInt64(hostBits))
    }

    func contains(_ other: IPPrefix) -> Bool {
        guard family == other.family, length <= other.length else { return false }
        let masked = IPPrefix.mask(
            high: other.high,
            low: other.low,
            length: length,
            bitWidth: family.bitWidth
        )
        return masked.high == high && masked.low == low
    }

    func overlaps(_ other: IPPrefix) -> Bool {
        contains(other) || other.contains(self)
    }

    /// The parent block, when this prefix and `sibling` are the two halves of
    /// one block, and nil otherwise.
    func merged(with sibling: IPPrefix) -> IPPrefix? {
        guard family == sibling.family, length == sibling.length, length > 0, self != sibling else {
            return nil
        }
        let parentLength = length - 1
        let ours = IPPrefix.mask(high: high, low: low, length: parentLength, bitWidth: family.bitWidth)
        let theirs = IPPrefix.mask(
            high: sibling.high,
            low: sibling.low,
            length: parentLength,
            bitWidth: family.bitWidth
        )
        guard ours == theirs else { return nil }
        return IPPrefix(family: family, high: ours.high, low: ours.low, length: parentLength)
    }

    /// What remains of this block once `exclusion` is carved out of it.
    ///
    /// This mirrors the Android client's RoutePolicy.RouteSpec.subtract, which
    /// has to express exclusions as a set of included routes because
    /// VpnService.Builder offers no exclusion list. iOS does not need it to
    /// build settings — NEIPv4Settings takes excludedRoutes directly — but the
    /// two platforms must agree on what an exclusion means, and this is the
    /// side that has tests.
    func subtracting(_ exclusion: IPPrefix) -> [IPPrefix] {
        guard family == exclusion.family, overlaps(exclusion) else { return [self] }
        guard exclusion.length > length else { return [] }
        let childLength = length + 1
        let halves = [
            IPPrefix(family: family, high: high, low: low, length: childLength),
            IPPrefix(
                family: family,
                high: high,
                low: low,
                length: childLength
            ).settingBit(childLength - 1)
        ]
        return halves.flatMap { $0.subtracting(exclusion) }
    }

    /// Parses a list of user-typed entries. Blank entries are ignored; every
    /// other one either parses or is returned, trimmed, in `rejected` so the
    /// caller can say which entry was wrong instead of how many.
    static func parseList(_ entries: [String]) -> (parsed: [IPPrefix], rejected: [String]) {
        var parsed: [IPPrefix] = []
        var rejected: [String] = []
        for entry in entries {
            let trimmed = entry.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty { continue }
            if let prefix = IPPrefix(cidr: trimmed) {
                parsed.append(prefix)
            } else {
                rejected.append(trimmed)
            }
        }
        return (parsed, rejected)
    }

    /// Collapses a set into the smallest equivalent one: subnets are absorbed
    /// into the blocks that already cover them, and sibling halves become their
    /// parent. The result is sorted.
    static func coalesce(_ prefixes: [IPPrefix]) -> [IPPrefix] {
        var stack: [IPPrefix] = []
        for prefix in prefixes.sorted() {
            var candidate = prefix
            var absorbed = false
            while let top = stack.last {
                if top.contains(candidate) {
                    absorbed = true
                    break
                }
                guard let parent = top.merged(with: candidate) else { break }
                stack.removeLast()
                candidate = parent
            }
            if !absorbed { stack.append(candidate) }
        }
        return stack
    }

    /// Dotted quad for IPv4, canonical compressed form for IPv6.
    var addressText: String {
        if family == .ipv4 {
            let value = UInt32(truncatingIfNeeded: low)
            return "\(value >> 24).\((value >> 16) & 0xFF).\((value >> 8) & 0xFF).\(value & 0xFF)"
        }
        var address = in6_addr()
        withUnsafeMutableBytes(of: &address) { raw in
            for index in 0..<8 { raw[index] = UInt8(truncatingIfNeeded: high >> (56 - 8 * UInt64(index))) }
            for index in 0..<8 { raw[8 + index] = UInt8(truncatingIfNeeded: low >> (56 - 8 * UInt64(index))) }
        }
        var buffer = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
        let rendered = withUnsafePointer(to: &address) { pointer in
            inet_ntop(AF_INET6, pointer, &buffer, socklen_t(INET6_ADDRSTRLEN))
        }
        guard rendered != nil else { return "::" }
        return String(cString: buffer)
    }

    /// The IPv4 subnet mask NEIPv4Route asks for, e.g. "255.255.240.0".
    var subnetMaskText: String {
        let value: UInt32 = length == 0 ? 0 : ~UInt32(0) << UInt32(32 - length)
        return "\(value >> 24).\((value >> 16) & 0xFF).\((value >> 8) & 0xFF).\(value & 0xFF)"
    }

    var cidrText: String { "\(addressText)/\(length)" }

    private func settingBit(_ index: Int) -> IPPrefix {
        let offset = family.bitWidth - 1 - index
        if offset >= 64 {
            return IPPrefix(family: family, high: high | (UInt64(1) << UInt64(offset - 64)), low: low, length: length)
        }
        return IPPrefix(family: family, high: high, low: low | (UInt64(1) << UInt64(offset)), length: length)
    }

    private static func mask(
        high: UInt64,
        low: UInt64,
        length: Int,
        bitWidth: Int
    ) -> (high: UInt64, low: UInt64) {
        let hostBits = bitWidth - length
        if hostBits <= 0 { return (high, low) }
        if hostBits >= bitWidth { return (0, 0) }
        if bitWidth == 32 {
            return (0, low & (~UInt64(0) << UInt64(hostBits)) & 0xFFFF_FFFF)
        }
        if hostBits >= 64 {
            let highHostBits = hostBits - 64
            let maskedHigh = highHostBits >= 64 ? 0 : high & (~UInt64(0) << UInt64(highHostBits))
            return (maskedHigh, 0)
        }
        return (high, low & (~UInt64(0) << UInt64(hostBits)))
    }

    private static func parseAddress(_ text: String) -> RawAddress? {
        var fourByte = in_addr()
        if text.contains(".") && !text.contains(":") {
            guard inet_pton(AF_INET, text, &fourByte) == 1 else { return nil }
            return RawAddress(family: .ipv4, high: 0, low: UInt64(UInt32(bigEndian: fourByte.s_addr)))
        }
        var sixteenByte = in6_addr()
        guard inet_pton(AF_INET6, text, &sixteenByte) == 1 else { return nil }
        var high: UInt64 = 0
        var low: UInt64 = 0
        withUnsafeBytes(of: &sixteenByte) { raw in
            for index in 0..<8 { high = (high << 8) | UInt64(raw[index]) }
            for index in 8..<16 { low = (low << 8) | UInt64(raw[index]) }
        }
        return RawAddress(family: .ipv6, high: high, low: low)
    }
}
