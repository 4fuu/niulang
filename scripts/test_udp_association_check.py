import argparse
import unittest

from scripts import udp_association_check as check


class EndpointTests(unittest.TestCase):
    def test_parse_ipv4_and_ipv6_endpoints(self):
        self.assertEqual(check.parse_endpoint("127.0.0.1:1080"), ("127.0.0.1", 1080))
        self.assertEqual(check.parse_endpoint("[2001:db8::1]:53"), ("2001:db8::1", 53))

    def test_parse_endpoint_rejects_bad_ports(self):
        for endpoint in ("missing-port", "host:nope", "host:0", "host:65536"):
            with self.subTest(endpoint=endpoint):
                with self.assertRaises(argparse.ArgumentTypeError):
                    check.parse_endpoint(endpoint)


class SocksAddressTests(unittest.TestCase):
    def test_round_trip_supported_address_types(self):
        for host in ("192.0.2.5", "2001:db8::5", "example.com"):
            with self.subTest(host=host):
                encoded = check.encode_address(host, 5353)
                decoded_host, decoded_port, consumed = check.decode_address(encoded)
                self.assertEqual(decoded_host, host)
                self.assertEqual(decoded_port, 5353)
                self.assertEqual(consumed, len(encoded))

    def test_decode_rejects_truncated_and_empty_addresses(self):
        invalid = (b"", b"\x01\x7f", b"\x04" + b"\0" * 10, b"\x03", b"\x03\x00\x00\x35")
        for encoded in invalid:
            with self.subTest(encoded=encoded):
                with self.assertRaises(ValueError):
                    check.decode_address(encoded)


class DNSNameTests(unittest.TestCase):
    def test_encode_dns_name(self):
        self.assertEqual(
            check.encode_dns_name("www.example.com."),
            b"\x03www\x07example\x03com\x00",
        )

    def test_encode_dns_name_rejects_empty_labels(self):
        for name in ("", ".", "www..example.com"):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    check.encode_dns_name(name)


if __name__ == "__main__":
    unittest.main()
