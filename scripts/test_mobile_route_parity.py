"""The tunnel constants the two mobile clients declare separately.

iOS and Android build the same tunnel through APIs that share no code:
NEPacketTunnelNetworkSettings on one side, VpnService.Builder on the
other, with the Go packet stack underneath both. Every value they must
agree on is therefore written twice, and today the only thing holding
them together is a comment in RoutePlan.swift saying "the two must not
drift".

Drift here is quiet. A local-exclusion list that gains an entry on one
platform is a route that reaches the LAN on one phone and the gateway on
another; an MTU that changes on one side fragments every flow on that
side alone. None of it fails a build, and neither client can see the
other. This is the check that does.

Update a value on purpose by updating it in every file listed here.
"""

import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
IOS_SETTINGS = ROOT / "mobile/ios/Shared/TunnelNetworkSettings.swift"
IOS_ROUTES = ROOT / "mobile/ios/Shared/RoutePlan.swift"
ANDROID_VPN = (
    ROOT / "mobile/android/app/src/debug/java/io/github/bojieli/queqiao"
    "/QueqiaoVpnService.java"
)
ANDROID_ROUTES = (
    ROOT / "mobile/android/app/src/debug/java/io/github/bojieli/queqiao"
    "/RoutePolicy.java"
)
GO_PACKET_STACK = ROOT / "mobile/core/packetstack.go"


def read(path):
    return path.read_text(encoding="utf-8")


def bracketed_strings(source, marker, closing):
    """The quoted entries of a list literal introduced by `marker`."""
    start = source.index(marker) + len(marker)
    return re.findall(r'"([^"]+)"', source[start : source.index(closing, start)])


def integer(source, pattern):
    match = re.search(pattern, source)
    assert match, f"no match for {pattern!r}"
    return int(match.group(1).replace("_", ""))


class MobileTunnelParityTests(unittest.TestCase):
    def test_the_local_exclusion_lists_are_the_same_list(self):
        ios = bracketed_strings(read(IOS_ROUTES), "static let localNetworks = [", "]")
        android = bracketed_strings(read(ANDROID_ROUTES), "LOCAL_EXCLUSIONS = {", "}")
        self.assertEqual(ios, android)
        # A guard against a parse that silently found nothing on both sides.
        self.assertIn("192.168.0.0/16", ios)
        self.assertIn("fe80::/10", ios)

    def test_the_tunnel_mtu_is_one_number_in_three_languages(self):
        ios = integer(read(IOS_SETTINGS), r"static let mtu: Int64 = ([\d_]+)")
        android = integer(read(ANDROID_VPN), r"int MTU = ([\d_]+);")
        core = integer(read(GO_PACKET_STACK), r"defaultMTU\s+= ([\d_]+)")
        self.assertEqual((ios, android), (core, core))
        # 1280 is the IPv6 minimum every path must carry without fragmenting.
        # Raising it is a measurement, not an edit.
        self.assertEqual(core, 1280)

    def test_both_clients_resolve_through_the_same_servers(self):
        ios = bracketed_strings(read(IOS_SETTINGS), "static let dnsServers = [", "]")
        android = re.findall(r'addDnsServer\("([^"]+)"\)', read(ANDROID_VPN))
        self.assertEqual(ios, android)
        self.assertTrue(ios, "no resolvers were parsed from either client")

    def test_the_interface_addresses_do_not_diverge(self):
        settings = read(IOS_SETTINGS)
        ios = [
            re.search(r'static let ipv4Address = "([^"]+)"', settings).group(1),
            re.search(r'static let ipv6Address = "([^"]+)"', settings).group(1),
        ]
        android = re.findall(r'addAddress\("([^"]+)", \d+\)', read(ANDROID_VPN))
        self.assertEqual(ios, android)


if __name__ == "__main__":
    unittest.main()
