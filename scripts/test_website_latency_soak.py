import subprocess
import unittest
from unittest import mock

from scripts import website_latency_soak as soak


class TargetTests(unittest.TestCase):
    def test_target_requires_safe_name_and_https(self):
        self.assertEqual(
            soak.parse_target("example=https://example.com/status"),
            ("example", "https://example.com/status"),
        )
        for value in ("https://example.com", "bad name=https://example.com", "x=http://example.com"):
            with self.subTest(value=value):
                with self.assertRaises(Exception):
                    soak.parse_target(value)


class CurlTests(unittest.TestCase):
    def test_parse_curl_output_converts_timings_to_milliseconds(self):
        self.assertEqual(
            soak.parse_curl_output("204\t0.001\t0.200\t0.250\t0.251\t0\n"),
            {
                "http_status": 204,
                "local_proxy_connect_ms": 1.0,
                "tls_ready_ms": 200.0,
                "ttfb_ms": 250.0,
                "total_ms": 251.0,
                "bytes_downloaded": 0,
            },
        )

    @mock.patch("scripts.website_latency_soak.subprocess.run")
    def test_probe_forces_remote_dns_through_socks(self, run):
        run.return_value = subprocess.CompletedProcess(
            [], 0, "204\t0.001\t0.200\t0.250\t0.251\t0\n", ""
        )
        result = soak.run_probe(
            "/usr/bin/curl",
            ("127.0.0.1", 12080),
            ("google", "https://www.google.com/generate_204"),
            2,
            5,
        )
        command = run.call_args.args[0]
        self.assertIn("socks5h://127.0.0.1:12080", command)
        self.assertEqual(command[command.index("--noproxy") + 1], "")
        self.assertEqual(result["status"], "ok")


class SummaryTests(unittest.TestCase):
    def test_summary_reports_correlated_failures_and_latency_gate(self):
        events = [
            {"round": 1, "target": "a", "elapsed_seconds": 0, "status": "ok", "ttfb_ms": 100},
            {"round": 1, "target": "b", "elapsed_seconds": 1, "status": "ok", "ttfb_ms": 200},
            {"round": 2, "target": "a", "elapsed_seconds": 60, "status": "failed"},
            {"round": 2, "target": "b", "elapsed_seconds": 61, "status": "failed"},
        ]
        summary = soak.build_summary(events, ["a", "b"], True, False, 120, 0.5, 150)
        self.assertEqual(summary["rounds_with_all_targets_failed"], 1)
        self.assertEqual(summary["targets"]["a"]["success_rate"], 0.5)
        self.assertFalse(summary["stable"])

    def test_percentile_interpolates(self):
        self.assertEqual(soak.percentile([100, 200], 50), 150)
        self.assertIsNone(soak.percentile([], 95))


if __name__ == "__main__":
    unittest.main()
