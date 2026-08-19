import importlib.util
import ipaddress
import pathlib
import unittest


SCRIPT = pathlib.Path(__file__).with_name("generate_cn_geoip.py")
_spec = importlib.util.spec_from_file_location("generate_cn_geoip", SCRIPT)
geoip = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(geoip)


SAMPLE = """\
2|apnic|20260101|88944|19830613|20260101|+1000
apnic|CN|ipv4|1.0.1.0|256|20110414|allocated
apnic|CN|ipv4|1.0.2.0|512|20110414|allocated
apnic|CN|ipv4|103.0.0.0|1024|20110414|assigned
apnic|JP|ipv4|1.0.16.0|4096|20110412|allocated
apnic|CN|ipv4|1.1.1.0|256|20110414|reserved
apnic|CN|ipv6|2001:250::|35|20110414|allocated
apnic|KR|ipv6|2001:230::|32|20110414|allocated
# a comment

apnic|CN|ipv4|malformed
"""


class ParseDelegationTests(unittest.TestCase):
    def test_selects_only_the_requested_country_and_live_statuses(self):
        networks = geoip.parse_delegation(SAMPLE, "CN")

        self.assertEqual(
            sorted(str(n) for n in networks),
            ["1.0.1.0/24", "1.0.2.0/23", "103.0.0.0/22", "2001:250::/35"],
        )

    def test_a_host_count_that_is_not_a_power_of_two_becomes_several_blocks(self):
        # The registry gives a count, not a prefix length. 768 hosts from
        # 1.0.1.0 is a /24 followed by a /23; no single prefix expresses it.
        networks = geoip.parse_delegation("apnic|CN|ipv4|1.0.1.0|768|x|allocated", "CN")

        self.assertEqual([str(n) for n in networks], ["1.0.1.0/24", "1.0.2.0/23"])

    def test_an_unaligned_range_still_covers_exactly_the_delegated_hosts(self):
        networks = geoip.parse_delegation("apnic|CN|ipv4|1.0.1.128|384|x|allocated", "CN")

        self.assertEqual(sum(n.num_addresses for n in networks), 384)
        self.assertEqual(str(networks[0].network_address), "1.0.1.128")

    def test_ignores_headers_comments_and_short_rows(self):
        self.assertEqual(geoip.parse_delegation("", "CN"), [])
        self.assertEqual(geoip.parse_delegation("# only a comment", "CN"), [])
        self.assertEqual(geoip.parse_delegation("apnic|CN|ipv4|1.0.1.0", "CN"), [])


class EncodingTests(unittest.TestCase):
    def round_trip(self, entries):
        networks = [ipaddress.ip_network(entry) for entry in entries]
        return [str(n) for n in geoip.decode(geoip.encode(networks))]

    def test_round_trip_preserves_both_families_sorted(self):
        self.assertEqual(
            self.round_trip(["2001:db8::/32", "203.0.113.0/24", "10.0.0.0/8"]),
            ["10.0.0.0/8", "203.0.113.0/24", "2001:db8::/32"],
        )

    def test_encode_collapses_before_writing(self):
        self.assertEqual(self.round_trip(["10.0.0.0/9", "10.128.0.0/9"]), ["10.0.0.0/8"])
        self.assertEqual(self.round_trip(["10.0.0.0/8", "10.1.0.0/16"]), ["10.0.0.0/8"])

    def test_boundary_prefixes_survive(self):
        self.assertEqual(
            self.round_trip(["0.0.0.0/0", "::/0"]),
            ["0.0.0.0/0", "::/0"],
        )
        self.assertEqual(
            self.round_trip(["203.0.113.7/32", "2001:db8::1/128"]),
            ["203.0.113.7/32", "2001:db8::1/128"],
        )

    def test_decode_rejects_a_foreign_or_truncated_blob(self):
        blob = geoip.encode([ipaddress.ip_network("10.0.0.0/8")])
        with self.assertRaises(ValueError):
            geoip.decode(b"NOPE" + blob[4:])
        with self.assertRaises(ValueError):
            geoip.decode(blob[:-1])
        with self.assertRaises(ValueError):
            geoip.decode(blob[:4] + b"\x02" + blob[5:])


class AggregateTests(unittest.TestCase):
    def networks(self, entries):
        return [ipaddress.ip_network(entry) for entry in entries]

    def test_a_set_already_within_the_limit_is_untouched(self):
        entries = self.networks(["10.0.0.0/8", "192.168.0.0/16"])

        result, wasted = geoip.aggregate(entries, 5)

        self.assertEqual([str(n) for n in result], ["10.0.0.0/8", "192.168.0.0/16"])
        self.assertEqual(wasted, 0)

    def test_free_merges_are_taken_first(self):
        # 10.0/9 and 10.128/9 are the two halves of 10/8, so joining them costs
        # nothing and must be preferred over the gap next door.
        entries = self.networks(["10.0.0.0/9", "10.128.0.0/9", "11.0.0.0/8", "13.0.0.0/8"])

        result, wasted = geoip.aggregate(entries, 3)

        self.assertEqual([str(n) for n in result], ["10.0.0.0/8", "11.0.0.0/8", "13.0.0.0/8"])
        self.assertEqual(wasted, 0)

    def test_the_result_still_covers_every_input_address(self):
        entries = self.networks(
            [f"10.{index}.0.0/24" for index in range(0, 64, 2)]
        )

        result, wasted = geoip.aggregate(entries, 4)

        self.assertLessEqual(len(result), 4)
        for network in entries:
            self.assertTrue(
                any(covering.supernet_of(network) for covering in result),
                f"{network} fell out of the aggregated set",
            )
        self.assertGreater(wasted, 0)

    def test_reported_waste_matches_what_the_result_over_includes(self):
        entries = self.networks(["10.0.0.0/24", "10.0.2.0/24", "10.0.8.0/24"])

        result, wasted = geoip.aggregate(entries, 1)

        covered = sum(n.num_addresses for n in entries)
        self.assertEqual(sum(n.num_addresses for n in result) - covered, wasted)

    def test_ipv6_aggregates_without_overflowing_the_cost_metric(self):
        entries = self.networks([f"2001:db8:{index:x}::/48" for index in range(8)])

        result, wasted = geoip.aggregate(entries, 2)

        self.assertLessEqual(len(result), 2)
        self.assertEqual(
            sum(n.num_addresses for n in result) - sum(n.num_addresses for n in entries),
            wasted,
        )


if __name__ == "__main__":
    unittest.main()
