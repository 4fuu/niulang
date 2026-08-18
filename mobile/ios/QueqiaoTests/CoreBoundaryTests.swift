import XCTest
import Mobilecore
@testable import Queqiao

final class CoreBoundaryTests: XCTestCase {
    func testRejectsUnknownProfileFields() {
        let profile = "{\"version\":1,\"unknown\":true}"
        XCTAssertThrowsError(try MobileCore.validateProfile(profile))
    }

    func testRejectsMalformedInvitationWithoutNetworkAccess() {
        XCTAssertThrowsError(try MobileCore.validateInvitation("https://example.com/invite"))
    }

    func testCatalogNormalizesMissingSelectionAndDuplicateRecords() {
        let summary = ProfileSummary(
            version: 1,
            name: "Example",
            endpoint: "gateway.example:443",
            providerID: "provider",
            gatewayID: "gateway",
            accountID: "account",
            deviceID: "device",
            deviceName: "Phone",
            certificateExpiry: "2030-01-01T00:00:00Z"
        )
        let profile = StoredProfile(
            id: "first",
            secretAccount: "secret.first",
            displayName: "Example",
            summary: summary,
            trafficPolicy: .excludeLocalNetworks,
            importedAt: "2026-08-18T00:00:00Z"
        )
        var catalog = ProfileCatalog(
            selectedProfileID: "missing",
            profiles: [profile, profile]
        )

        catalog.normalize()

        XCTAssertEqual(catalog.profiles, [profile])
        XCTAssertEqual(catalog.selectedProfileID, profile.id)
    }

    func testTunnelMetricsDecodeCoreWireFormat() throws {
        let data = Data(
            """
            {"version":1,"state":"running","packets":{},
             "transport":{"BytesUp":2048,"BytesDown":4096,"ActiveFlows":3}}
            """.utf8
        )

        XCTAssertEqual(
            try TunnelMetrics.decode(data),
            TunnelMetrics(bytesUp: 2_048, bytesDown: 4_096, activeFlows: 3)
        )
    }

    func testDiagnosticSanitizerRedactsInvitationsAndSecrets() {
        let message = "open queqiao://enroll/private-payload token=secret-value"
        let sanitized = DiagnosticStore.sanitize(message)

        XCTAssertFalse(sanitized.contains("private-payload"))
        XCTAssertFalse(sanitized.contains("secret-value"))
        XCTAssertTrue(sanitized.contains("queqiao://<redacted>"))
        XCTAssertTrue(sanitized.contains("token=<redacted>"))
    }

    func testProviderEndpointParsesSupportedAddressForms() throws {
        XCTAssertEqual(try ProviderEndpoint.host(from: "203.0.113.8:443"), "203.0.113.8")
        XCTAssertEqual(try ProviderEndpoint.host(from: "gateway.example:8443"), "gateway.example")
        XCTAssertEqual(try ProviderEndpoint.host(from: "[2001:db8::1]:443"), "2001:db8::1")
        XCTAssertEqual(try ProviderEndpoint.resolvedAddress(from: "203.0.113.8:443"), "203.0.113.8")
    }

    func testProviderEndpointRejectsMalformedAddresses() {
        XCTAssertThrowsError(try ProviderEndpoint.host(from: "missing-port"))
        XCTAssertThrowsError(try ProviderEndpoint.host(from: "2001:db8::1:443"))
        XCTAssertThrowsError(try ProviderEndpoint.host(from: "example.com:0"))
        XCTAssertThrowsError(try ProviderEndpoint.host(from: "example.com:70000"))
    }

    func testTunnelStartCompletionIsDeliveredOnlyOnce() {
        var deliveredErrors: [Error?] = []
        let completion = OneShotErrorCompletion { deliveredErrors.append($0) }

        XCTAssertTrue(completion.call(nil))
        XCTAssertFalse(completion.call(TestError.lateFailure))
        XCTAssertEqual(deliveredErrors.count, 1)
        XCTAssertNil(deliveredErrors[0])
    }

    func testTunnelStopCompletionIsDeliveredOnlyOnce() {
        var deliveryCount = 0
        let completion = OneShotVoidCompletion { deliveryCount += 1 }

        XCTAssertTrue(completion.call())
        XCTAssertFalse(completion.call())
        XCTAssertEqual(deliveryCount, 1)
    }

    func testProfileProbeResultDecodesValidatedWireFormat() throws {
        let result = try ProfileProbeResult.decode(
            "{\"version\":1,\"transport\":\"quic\",\"latency_ms\":87}"
        )

        XCTAssertEqual(
            result,
            ProfileProbeResult(version: 1, transport: "quic", latencyMilliseconds: 87)
        )
        XCTAssertThrowsError(
            try ProfileProbeResult.decode(
                "{\"version\":2,\"transport\":\"quic\",\"latency_ms\":87}"
            )
        )
        XCTAssertThrowsError(
            try ProfileProbeResult.decode(
                "{\"version\":1,\"transport\":\"unknown\",\"latency_ms\":87}"
            )
        )
        XCTAssertThrowsError(
            try ProfileProbeResult.decode(
                "{\"version\":1,\"transport\":\"tcp\",\"latency_ms\":0}"
            )
        )
    }
}

private enum TestError: Error {
    case lateFailure
}
