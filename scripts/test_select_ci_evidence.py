import importlib.util
import pathlib
import unittest


SCRIPT = pathlib.Path(__file__).with_name("select_ci_evidence.py")
SPEC = importlib.util.spec_from_file_location("select_ci_evidence", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class SelectCIEvidenceTests(unittest.TestCase):
    sha = "a" * 40

    @staticmethod
    def make_run(run_id, run_number, **changes):
        run = {
            "id": run_id,
            "run_number": run_number,
            "html_url": f"https://github.com/example/project/actions/runs/{run_id}",
            "head_sha": "a" * 40,
            "conclusion": "success",
            "event": "push",
        }
        run.update(changes)
        return run

    def test_selects_newest_exact_sha_push_or_dispatch(self):
        document = {
            "workflow_runs": [
                self.make_run(10, 7),
                self.make_run(11, 8, event="workflow_dispatch"),
                self.make_run(12, 9, head_sha="b" * 40),
                self.make_run(13, 10, conclusion="failure"),
                self.make_run(14, 11, event="pull_request"),
            ]
        }

        self.assertEqual(
            MODULE.select_ci_run(document, self.sha),
            {
                "id": 11,
                "run_number": 8,
                "html_url": "https://github.com/example/project/actions/runs/11",
            },
        )

    def test_rejects_pull_request_wrong_sha_and_failed_runs(self):
        document = {
            "workflow_runs": [
                self.make_run(1, 1, event="pull_request"),
                self.make_run(2, 2, head_sha="b" * 40),
                self.make_run(3, 3, conclusion="failure"),
            ]
        }
        with self.assertRaisesRegex(ValueError, "no successful"):
            MODULE.select_ci_run(document, self.sha)

    def test_rejects_malformed_response_sha_and_run_metadata(self):
        with self.assertRaisesRegex(ValueError, "workflow_runs"):
            MODULE.select_ci_run({}, self.sha)
        with self.assertRaisesRegex(ValueError, "full lowercase SHA"):
            MODULE.select_ci_run({"workflow_runs": []}, "A" * 40)
        for changes in (
            {"id": "1"},
            {"run_number": 0},
            {"html_url": "javascript:alert(1)"},
        ):
            candidate = self.make_run(1, 1)
            candidate.update(changes)
            with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, "no successful"):
                MODULE.select_ci_run({"workflow_runs": [candidate]}, self.sha)


if __name__ == "__main__":
    unittest.main()
