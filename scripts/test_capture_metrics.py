import unittest

from scripts import capture_metrics


class CaptureMetricsTests(unittest.TestCase):
    def test_parse_metrics_accepts_labels_and_timestamps(self):
        result = capture_metrics.parse_metrics(
            """
# HELP ignored metadata
queqiao_active_flows 2
queqiao_quic_controller_kind{kind="bbr-tuic"} 1
queqiao_quic_smoothed_rtt_seconds 0.201 1787000000000
not_queqiao 9
broken nope
"""
        )
        self.assertEqual(
            result,
            {
                "queqiao_active_flows": 2.0,
                "queqiao_quic_controller_kind": 1.0,
                "queqiao_quic_smoothed_rtt_seconds": 0.201,
            },
        )

    def test_empty_or_malformed_lines_are_ignored(self):
        self.assertEqual(capture_metrics.parse_metrics("# comment\nqueqiao_bad nope\n"), {})


if __name__ == "__main__":
    unittest.main()
