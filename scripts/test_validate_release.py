import importlib.util
import pathlib
import tempfile
import unittest
import warnings
import zipfile


SCRIPT = pathlib.Path(__file__).with_name("validate_release.py")
SPEC = importlib.util.spec_from_file_location("validate_release", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class ValidateReleaseTests(unittest.TestCase):
    def test_checksums_reject_paths_and_duplicates(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory, "SHA256SUMS")
            path.write_text("0" * 64 + "  ../artifact\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                MODULE.parse_checksums(path)
            path.write_text(("0" * 64 + "  artifact\n") * 2, encoding="utf-8")
            with self.assertRaises(ValueError):
                MODULE.parse_checksums(path)
            path.write_text("z" * 64 + "  artifact\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                MODULE.parse_checksums(path)

    def test_buildinfo_rejects_duplicate_or_incomplete_keys(self):
        with self.assertRaises(ValueError):
            MODULE.parse_buildinfo(b"version=one\nversion=two\n")
        with self.assertRaises(ValueError):
            MODULE.parse_buildinfo(b"version=one\n")

    def test_archive_rejects_absolute_and_duplicate_members(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory, "bad.zip")
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("/root/file", b"bad")
            with self.assertRaises(ValueError):
                MODULE.archive_files(path, "root")
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", UserWarning)
                with zipfile.ZipFile(path, "w") as archive:
                    archive.writestr("root/file", b"one")
                    archive.writestr("root/file", b"two")
            with self.assertRaises(ValueError):
                MODULE.archive_files(path, "root")


if __name__ == "__main__":
    unittest.main()
