import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("check_gosec.py")


class GosecBaselineTests(unittest.TestCase):
    def run_report(self, issues):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            report = root / "report.json"
            report.write_text(json.dumps({"Issues": issues}), encoding="utf-8")
            return subprocess.run(
                [sys.executable, str(SCRIPT), str(report), str(root)],
                text=True,
                capture_output=True,
                check=False,
            )

    def test_accepts_reviewed_bucket(self):
        issue = {"rule_id": "G404", "file": "internal/pathsim/tcp.go"}
        result = self.run_report([issue, issue])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_accepts_only_the_two_reviewed_client_conversions(self):
        issue = {"rule_id": "G115", "file": "internal/pep/client.go"}
        accepted = self.run_report([issue, issue])
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        excess = self.run_report([issue, issue, issue])
        self.assertEqual(excess.returncode, 1)

    def test_rejects_new_rule_or_excess_count(self):
        new_rule = self.run_report([{"rule_id": "G999", "file": "new.go"}])
        self.assertEqual(new_rule.returncode, 1)
        issue = {"rule_id": "G404", "file": "internal/pathsim/tcp.go"}
        excess = self.run_report([issue, issue, issue])
        self.assertEqual(excess.returncode, 1)


if __name__ == "__main__":
    unittest.main()
