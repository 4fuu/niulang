import os
import pathlib
import subprocess
import tempfile
import unittest


REPOSITORY = pathlib.Path(__file__).resolve().parents[1]
MANAGER = REPOSITORY / "deploy" / "manage.sh"
CLIENT_INSTALLER = REPOSITORY / "deploy" / "install-client.sh"
SERVER_INSTALLER = REPOSITORY / "deploy" / "install-server.sh"


class DeployManagerTests(unittest.TestCase):
    def run_shell(
        self,
        body: str,
        root: pathlib.Path | None = None,
        extra_environment: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment["NIULANG_MANAGER_TESTING"] = "1"
        if root is not None:
            home = root / "home" / "operator"
            home.mkdir(parents=True, exist_ok=True)
            environment["HOME"] = str(home)
            environment["NIULANG_MANAGER_ROOT"] = str(root)
        if extra_environment:
            environment.update(extra_environment)
        return subprocess.run(
            ["sh", "-c", f'. "{MANAGER}"\n{body}'],
            cwd=REPOSITORY,
            env=environment,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_release_and_invitation_inputs_are_protocol_two_only(self):
        result = self.run_shell(
            """
            test "$(map_arch x86_64)" = amd64
            test "$(map_arch aarch64)" = arm64
            test "$(release_asset v2026.827.1 arm64)" = niulangd_v2026.827.1_linux_arm64.tar.gz
            valid_invitation niulang://enroll/token
            ! valid_invitation queqiao://enroll/token
            ! valid_invitation niulang://other/token
            validate_version v2026.827.1
            ! validate_version 0.2.0
            """
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_legacy_service_and_data_are_distinguished(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            provider = root / "var" / "lib" / "queqiao" / "provider"
            provider.mkdir(parents=True)
            result = self.run_shell(
                "legacy_role_found server; ! legacy_service_role_found server",
                root,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

            unit = root / "etc" / "systemd" / "system" / "queqiao-server.service"
            unit.parent.mkdir(parents=True)
            unit.write_text(
                "[Service]\nExecStart=/usr/local/bin/queqiaod server\n",
                encoding="utf-8",
            )
            result = self.run_shell(
                "legacy_service_role_found server; legacy_service_found",
                root,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_legacy_stop_restore_and_removal_are_separate_lifecycle_steps(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            unit = root / "etc" / "systemd" / "system" / "queqiao-server.service"
            unit.parent.mkdir(parents=True)
            unit.write_text("[Service]\n", encoding="utf-8")
            provider = root / "var" / "lib" / "queqiao" / "provider"
            provider.mkdir(parents=True)

            fake_bin = root / "fake-bin"
            fake_bin.mkdir()
            systemctl_log = root / "systemctl.log"
            systemctl = fake_bin / "systemctl"
            systemctl.write_text(
                "#!/bin/sh\n"
                'printf "%s\\n" "$*" >>"$SYSTEMCTL_LOG"\n'
                "exit 0\n",
                encoding="utf-8",
            )
            systemctl.chmod(0o755)
            snapshot = root / "snapshot.tsv"
            result = self.run_shell(
                'run_root() { "$@"; }\n'
                f'record_legacy_state server "{snapshot}"\n'
                "stop_legacy_role server\n"
                f'restore_legacy_state "{snapshot}"\n'
                "remove_legacy_role server\n"
                'test ! -f "$(legacy_system_unit_path queqiao-server)"\n'
                'test -d "$(root_path /var/lib/queqiao/provider)"',
                root,
                {
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                    "SYSTEMCTL_LOG": str(systemctl_log),
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            commands = systemctl_log.read_text(encoding="utf-8")
            self.assertIn("disable --now queqiao-server.service", commands)
            self.assertIn("enable queqiao-server.service", commands)
            self.assertIn("start queqiao-server.service", commands)
            self.assertIn("daemon-reload", commands)

    def test_purge_requires_confirmation_and_refuses_live_service(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            data = root / "var" / "lib" / "queqiao"
            data.mkdir(parents=True)
            result = self.run_shell(
                'run_root() { "$@"; }\nprintf "no\\n" | purge_legacy_data\n'
                'test -d "$(root_path /var/lib/queqiao)"',
                root,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

            unit = root / "etc" / "systemd" / "system" / "queqiao-server.service"
            unit.parent.mkdir(parents=True)
            unit.write_text("[Service]\n", encoding="utf-8")
            result = self.run_shell(
                'run_root() { "$@"; }\nprintf "DELETE QUEQIAO\\n" | '
                "(purge_legacy_data)",
                root,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(data.is_dir())

    def test_help_names_new_invitation_and_separate_delete_steps(self):
        result = subprocess.run(
            ["sh", str(MANAGER), "--help"],
            cwd=REPOSITORY,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("niulang://enroll/", result.stdout)
        self.assertIn("legacy-stop", result.stdout)
        self.assertIn("legacy-remove", result.stdout)
        self.assertIn("legacy-purge", result.stdout)

    def test_installers_reject_old_input_contracts(self):
        result = subprocess.run(
            [
                "sh",
                str(CLIENT_INSTALLER),
                "--invite",
                "niulang://other/token",
                "--dry-run",
            ],
            cwd=REPOSITORY,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not an invitation URI", result.stderr)

        result = subprocess.run(
            [
                "sh",
                str(SERVER_INSTALLER),
                "--user-max-sessions",
                "8",
                "--dry-run",
            ],
            cwd=REPOSITORY,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unknown argument: --user-max-sessions", result.stderr)

    def test_manager_has_no_old_release_compatibility_branch(self):
        source = MANAGER.read_text(encoding="utf-8")
        self.assertNotIn("v0.2.0", source)
        self.assertNotIn("wire=1", source)
        self.assertNotIn("queqiao://", source)


if __name__ == "__main__":
    unittest.main()
