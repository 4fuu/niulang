import pathlib
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("changelog.py")

HEADER = """# Changelog

Preamble that the tool must leave alone.

"""

RELEASED = """## v0.1.0 - 2026-08-19

### Added

- Something that already shipped.

"""


class ChangelogTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        (self.root / "changelog.d").mkdir()
        self.changelog = self.root / "CHANGELOG.md"
        self.changelog.write_text(HEADER + RELEASED, encoding="utf-8")

    def fragment(self, name, body):
        path = self.root / "changelog.d" / name
        path.write_text(body, encoding="utf-8")
        return path

    def run_tool(self, *arguments):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--root", str(self.root), *arguments],
            text=True,
            capture_output=True,
            check=False,
        )

    def test_check_accepts_a_well_formed_fragment(self):
        self.fragment("a-fix.fixed.md", "One sentence about the fix.\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 pending change", result.stdout)

    def test_check_rejects_a_malformed_name(self):
        self.fragment("Not A Slug.fixed.md", "Body.\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("<slug>.<category>.md", result.stderr)

    def test_check_rejects_an_unknown_category(self):
        self.fragment("a-fix.improved.md", "Body.\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("unknown category", result.stderr)

    def test_check_rejects_an_empty_fragment(self):
        self.fragment("a-fix.fixed.md", "\n\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("empty", result.stderr)

    def test_check_rejects_a_leading_list_marker(self):
        self.fragment("a-fix.fixed.md", "- Body already bulleted.\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("without a list marker", result.stderr)

    def test_check_rejects_crlf(self):
        path = self.root / "changelog.d" / "a-fix.fixed.md"
        path.write_bytes(b"Body.\r\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("LF line endings", result.stderr)

    def test_check_measures_width_after_rendering(self):
        # 79 columns of body become 81 once the bullet marker is added.
        self.fragment("a-fix.fixed.md", "x" * 79 + "\n")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("81 columns once rendered", result.stderr)

    def test_check_leaves_fenced_blocks_unwrapped(self):
        body = "Run it:\n\n```sh\n" + "queqiaod --flag " * 8 + "\n```\n"
        self.fragment("a-fix.fixed.md", body)
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_check_rejects_an_unreleased_section(self):
        self.changelog.write_text(
            HEADER + "## Unreleased\n\n" + RELEASED, encoding="utf-8"
        )
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("changelog.d/", result.stderr)

    def test_check_rejects_out_of_order_sections(self):
        older = "## v0.0.9 - 2026-08-01\n\n### Fixed\n\n- Older.\n\n"
        self.changelog.write_text(HEADER + older + RELEASED, encoding="utf-8")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("not below the section above it", result.stderr)

    def test_check_rejects_an_impossible_date(self):
        broken = "## v0.2.0 - 2026-02-31\n\n### Fixed\n\n- Nope.\n\n"
        self.changelog.write_text(HEADER + broken + RELEASED, encoding="utf-8")
        result = self.run_tool("check")
        self.assertEqual(result.returncode, 1)
        self.assertIn("not a calendar date", result.stderr)

    def test_release_groups_orders_and_indents(self):
        self.fragment("zeta.added.md", "Zeta arrived.\n")
        self.fragment("alpha.added.md", "Alpha arrived.\n")
        self.fragment(
            "wrapped.fixed.md", "First line of the entry.\nSecond line of it.\n"
        )
        result = self.run_tool("release", "--version", "v0.2.0", "--date", "2026-08-24")
        self.assertEqual(result.returncode, 0, result.stderr)
        text = self.changelog.read_text(encoding="utf-8")
        self.assertIn(
            "## v0.2.0 - 2026-08-24\n\n### Added\n\n"
            "- Alpha arrived.\n- Zeta arrived.\n\n"
            "### Fixed\n\n"
            "- First line of the entry.\n  Second line of it.\n",
            text,
        )
        self.assertTrue(text.startswith(HEADER))
        self.assertIn(RELEASED, text)
        self.assertEqual(
            sorted(p.name for p in (self.root / "changelog.d").iterdir()), []
        )

    def test_release_refuses_a_released_version(self):
        self.fragment("a-fix.fixed.md", "Body.\n")
        result = self.run_tool("release", "--version", "v0.1.0")
        self.assertEqual(result.returncode, 1)
        self.assertIn("already released", result.stderr)
        self.assertTrue((self.root / "changelog.d" / "a-fix.fixed.md").exists())

    def test_release_refuses_an_empty_pending_set(self):
        result = self.run_tool("release", "--version", "v0.2.0")
        self.assertEqual(result.returncode, 1)
        self.assertIn("no pending changes", result.stderr)
        self.assertEqual(self.changelog.read_text(encoding="utf-8"), HEADER + RELEASED)

    def test_release_refuses_a_bad_version(self):
        self.fragment("a-fix.fixed.md", "Body.\n")
        result = self.run_tool("release", "--version", "0.2")
        self.assertEqual(result.returncode, 1)
        self.assertIn("vMAJOR.MINOR.PATCH", result.stderr)

    def test_release_refuses_while_a_fragment_is_invalid(self):
        self.fragment("a-fix.fixed.md", "Body.\n")
        self.fragment("broken.improved.md", "Body.\n")
        result = self.run_tool("release", "--version", "v0.2.0")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.changelog.read_text(encoding="utf-8"), HEADER + RELEASED)
        self.assertTrue((self.root / "changelog.d" / "a-fix.fixed.md").exists())

    def test_preview_changes_nothing(self):
        self.fragment("a-fix.fixed.md", "Body.\n")
        before = self.changelog.read_text(encoding="utf-8")
        result = self.run_tool("preview")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("## Unreleased\n\n### Fixed\n\n- Body.", result.stdout)
        self.assertEqual(self.changelog.read_text(encoding="utf-8"), before)
        self.assertTrue((self.root / "changelog.d" / "a-fix.fixed.md").exists())

    def test_new_wraps_prose_to_the_fragment_width(self):
        sentence = "The gateway now refuses an unreachable endpoint politely. "
        result = self.run_tool(
            "new", "fixed", "polite-refusal", "-m", sentence * 4
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        body = (self.root / "changelog.d" / "polite-refusal.fixed.md").read_text(
            encoding="utf-8"
        )
        self.assertTrue(all(len(line) <= 78 for line in body.split("\n")), body)
        self.assertEqual(self.run_tool("check").returncode, 0)

    def test_new_refuses_to_overwrite(self):
        self.fragment("taken.fixed.md", "Original.\n")
        result = self.run_tool("new", "fixed", "taken", "-m", "Replacement.")
        self.assertEqual(result.returncode, 1)
        self.assertIn("already exists", result.stderr)
        self.assertEqual(
            (self.root / "changelog.d" / "taken.fixed.md").read_text(encoding="utf-8"),
            "Original.\n",
        )

    def test_new_refuses_an_unknown_category(self):
        result = self.run_tool("new", "improved", "a-slug", "-m", "Body.")
        self.assertEqual(result.returncode, 1)
        self.assertIn("unknown category", result.stderr)


class RepositoryTests(unittest.TestCase):
    def test_the_repository_state_is_valid(self):
        root = SCRIPT.resolve().parent.parent
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--root", str(root), "check"],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
