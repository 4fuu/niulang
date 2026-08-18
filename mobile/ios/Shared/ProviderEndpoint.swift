import Darwin
import Foundation

enum ProviderEndpoint {
    static func host(from endpoint: String) throws -> String {
        let host: Substring
        let portText: Substring
        if endpoint.first == "[" {
            guard let closingBracket = endpoint.firstIndex(of: "]"),
                  endpoint.index(after: closingBracket) < endpoint.endIndex,
                  endpoint[endpoint.index(after: closingBracket)] == ":" else {
                throw ProviderEndpointError.invalidEndpoint
            }
            host = endpoint[endpoint.index(after: endpoint.startIndex)..<closingBracket]
            portText = endpoint[endpoint.index(closingBracket, offsetBy: 2)...]
        } else {
            guard let separator = endpoint.lastIndex(of: ":") else {
                throw ProviderEndpointError.invalidEndpoint
            }
            host = endpoint[..<separator]
            portText = endpoint[endpoint.index(after: separator)...]
            guard !host.contains(":") else {
                throw ProviderEndpointError.invalidEndpoint
            }
        }
        guard !host.isEmpty,
              let port = Int(portText),
              (1...65_535).contains(port) else {
            throw ProviderEndpointError.invalidEndpoint
        }
        return String(host)
    }

    static func resolvedAddress(from endpoint: String) throws -> String {
        let host = try host(from: endpoint)
        var hints = addrinfo()
        hints.ai_flags = AI_ADDRCONFIG
        hints.ai_family = AF_UNSPEC
        hints.ai_socktype = SOCK_STREAM
        hints.ai_protocol = IPPROTO_TCP

        var results: UnsafeMutablePointer<addrinfo>?
        let status = getaddrinfo(host, nil, &hints, &results)
        guard status == 0, let first = results else {
            let detail = status == 0 ? "no addresses" : String(cString: gai_strerror(status))
            throw ProviderEndpointError.resolutionFailed(host: host, detail: detail)
        }
        defer { freeaddrinfo(first) }

        var current: UnsafeMutablePointer<addrinfo>? = first
        while let candidate = current {
            defer { current = candidate.pointee.ai_next }
            guard candidate.pointee.ai_family == AF_INET || candidate.pointee.ai_family == AF_INET6,
                  let address = candidate.pointee.ai_addr else { continue }
            var buffer = [CChar](repeating: 0, count: Int(NI_MAXHOST))
            let nameStatus = getnameinfo(
                address,
                candidate.pointee.ai_addrlen,
                &buffer,
                socklen_t(buffer.count),
                nil,
                0,
                NI_NUMERICHOST
            )
            if nameStatus == 0 {
                let terminator = buffer.firstIndex(of: 0) ?? buffer.endIndex
                let bytes = buffer[..<terminator].map { UInt8(bitPattern: $0) }
                if let resolved = String(bytes: bytes, encoding: .utf8) {
                    return resolved
                }
            }
        }
        throw ProviderEndpointError.resolutionFailed(host: host, detail: "no IP address")
    }
}

enum ProviderEndpointError: LocalizedError {
    case invalidEndpoint
    case resolutionFailed(host: String, detail: String)

    var errorDescription: String? {
        switch self {
        case .invalidEndpoint:
            return "The selected profile has an invalid provider endpoint."
        case let .resolutionFailed(host, detail):
            return "Could not resolve provider endpoint \(host): \(detail)."
        }
    }
}
