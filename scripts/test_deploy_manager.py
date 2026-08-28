import json
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
        environment["NIULANG_INIT_SYSTEM"] = "systemd"
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

    def test_legacy_status_allows_migration_to_continue(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            unit = root / "etc" / "systemd" / "system" / "queqiao-server.service"
            unit.parent.mkdir(parents=True)
            unit.write_text("[Service]\n", encoding="utf-8")

            result = self.run_shell(
                'show_legacy_status\nprintf "migration-ready\\n"',
                root,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("migration-ready", result.stdout)

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
                'test ! -f "$(system_unit_path queqiao-server)"\n'
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

    def test_client_installer_preserves_existing_runtime_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            home = pathlib.Path(directory)
            config = home / ".config" / "niulang"
            config.mkdir(parents=True)
            manifest = config / "providers.json"
            manifest.write_text(
                '{"version":1,"providers":[{"name":"primary","profile":"primary.json","listen":"127.0.0.1:12080"}]}\n',
                encoding="utf-8",
            )
            service_dir = home / ".config" / "systemd" / "user"
            service_dir.mkdir(parents=True)
            unit = service_dir / "niulang-client.service"
            unit.write_text(
                f'[Service]\nExecStart="{home}/.niulang/bin/niulangd" "client" '
                f'"--providers" "{manifest}" "--local-address" "if:eth1" '
                '"--max-sessions" "4096" "--log-level" "warn" '
                '"--metrics-listen" "127.0.0.1:12999"\n',
                encoding="utf-8",
            )
            result = subprocess.run(
                ["sh", str(CLIENT_INSTALLER), "--dry-run"],
                cwd=REPOSITORY,
                env={**os.environ, "HOME": str(home)},
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--local-address if:eth1", result.stdout)
            self.assertIn("shared maximum of 4096", result.stdout)
            self.assertIn("metrics at 127.0.0.1:12999", result.stdout)
            self.assertIn("log level warn", result.stdout)

    def test_manager_has_no_old_release_compatibility_branch(self):
        source = MANAGER.read_text(encoding="utf-8")
        self.assertNotIn("v0.2.0", source)
        self.assertNotIn("wire=1", source)
        self.assertNotIn("queqiao://", source)

    def test_openwrt_defaults_and_service_rendering(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            result = self.run_shell(
                """
                detect_init_system
                test "$INIT_SYSTEM" = openwrt
                test "$NATIVE_BINARY" = /usr/bin/niulangd
                test "$DATA_DIR" = /etc/niulang
                test "$RUNTIME_USER" = root
                SERVER_PORT=8443
                SERVER_TRANSPORT=auto
                SERVER_MAX_SESSIONS=4096
                SERVER_METRICS_PORT=19090
                render_openwrt_service server >"$NIULANG_MANAGER_ROOT/server"
                CLIENT_MODE=single
                CLIENT_LOCAL_ADDRESS=openwrt:wan
                OPENWRT_NETWORK=wan
                CLIENT_MAX_SESSIONS=2048
                CLIENT_METRICS_PORT=12090
                render_openwrt_service client >"$NIULANG_MANAGER_ROOT/client"
                """,
                root,
                {"NIULANG_INIT_SYSTEM": "openwrt"},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            server = (root / "server").read_text(encoding="utf-8")
            client = (root / "client").read_text(encoding="utf-8")
            self.assertIn('procd_set_param command "$PROG" "server"', server)
            self.assertIn('command --listen ":8443"', server)
            self.assertIn('command --transport "auto"', server)
            self.assertIn('local local_address="openwrt:wan"', client)
            self.assertIn('network_get_ipaddr local_address "wan"', client)
            self.assertIn("procd_add_interface_trigger", client)
            self.assertIn("/etc/init.d/niulang-client restart", client)

    def test_openrc_service_rendering_uses_runtime_account(self):
        result = self.run_shell(
            """
            detect_init_system
            test "$INIT_SYSTEM" = openrc
            test "$NATIVE_BINARY" = /usr/local/bin/niulangd
            render_openrc_service client '--profile /var/lib/niulang/client.json --max-sessions 2048'
            """,
            extra_environment={"NIULANG_INIT_SYSTEM": "openrc"},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('command="/usr/local/bin/niulangd"', result.stdout)
        self.assertIn('command_user="niulang:niulang"', result.stdout)
        self.assertIn("supervisor=supervise-daemon", result.stdout)

    def test_release_source_is_validated_persisted_and_reloaded(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            result = self.run_shell(
                """
                run_root() { "$@"; }
                detect_init_system
                set_release_source example/niulang true
                save_release_source
                set_release_source 4fuu/niulang false
                RELEASE_SOURCE_EXPLICIT=false
                load_saved_release_source
                test "$REPOSITORY" = example/niulang
                test "$RELEASE_INCLUDE_PRERELEASE" = true
                ! valid_repository 'https://github.com/example/niulang'
                """,
                root,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            source = root / "etc" / "niulang" / "release-source"
            self.assertEqual(
                source.read_text(encoding="utf-8"),
                "repository=example/niulang\ninclude_prerelease=true\n",
            )

    def test_native_manifest_create_and_append_remain_valid_json(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            manifest = root / "providers.json"
            appended = root / "providers-appended.json"
            result = self.run_shell(
                f"""
                create_provider_manifest '{manifest}' primary /etc/niulang/client.json 12080 secondary /etc/niulang/client-secondary.json 12081
                append_provider_manifest '{manifest}' '{appended}' tertiary /etc/niulang/client-tertiary.json 12082
                """,
                root,
                {"NIULANG_INIT_SYSTEM": "openwrt"},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            payload = json.loads(appended.read_text(encoding="utf-8"))
            self.assertEqual(payload["version"], 1)
            self.assertEqual(
                [provider["name"] for provider in payload["providers"]],
                ["primary", "secondary", "tertiary"],
            )

    def test_openwrt_legacy_stop_restore_and_remove(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            init = root / "etc" / "init.d" / "queqiao-client"
            init.parent.mkdir(parents=True)
            log = root / "legacy.log"
            init.write_text(
                "#!/bin/sh\n"
                'printf "%s\\n" "$1" >>"$LEGACY_LOG"\n'
                'case "$1" in enabled|status) exit 0;; esac\n'
                "exit 0\n",
                encoding="utf-8",
            )
            init.chmod(0o755)
            fake_bin = root / "fake-bin"
            fake_bin.mkdir()
            (fake_bin / "ubus").write_text(
                "#!/bin/sh\nprintf '%s\\n' '{\"running\":true}'\n",
                encoding="utf-8",
            )
            (fake_bin / "ubus").chmod(0o755)
            snapshot = root / "snapshot.tsv"
            result = self.run_shell(
                f"""
                run_root() {{ "$@"; }}
                record_legacy_state client '{snapshot}'
                stop_legacy_role client
                restore_legacy_state '{snapshot}'
                remove_legacy_role client
                test ! -f '{init}'
                """,
                root,
                {
                    "NIULANG_INIT_SYSTEM": "openwrt",
                    "LEGACY_LOG": str(log),
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            actions = log.read_text(encoding="utf-8")
            self.assertIn("disable", actions)
            self.assertIn("enable", actions)
            self.assertIn("stop", actions)
            self.assertIn("start", actions)

    def test_native_client_configuration_round_trips_shared_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            service = root / "etc" / "init.d" / "niulang-client"
            service.parent.mkdir(parents=True)
            result = self.run_shell(
                """
                detect_init_system
                CLIENT_MODE=multi
                CLIENT_LOCAL_ADDRESS=openwrt:wan2
                OPENWRT_NETWORK=wan2
                CLIENT_MAX_SESSIONS=3072
                CLIENT_METRICS_PORT=12091
                render_openwrt_service client >"$(native_service_path client)"
                CLIENT_MODE=single
                CLIENT_LOCAL_ADDRESS=auto
                CLIENT_MAX_SESSIONS=1
                CLIENT_METRICS_PORT=1
                load_native_client_configuration
                test "$CLIENT_MODE" = multi
                test "$CLIENT_LOCAL_ADDRESS" = openwrt:wan2
                test "$OPENWRT_NETWORK" = wan2
                test "$CLIENT_MAX_SESSIONS" = 3072
                test "$CLIENT_METRICS_PORT" = 12091
                test "$PROVIDERS_PATH" = /etc/niulang/providers.json
                """,
                root,
                {"NIULANG_INIT_SYSTEM": "openwrt"},
            )
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_systemd_client_limit_update_preserves_other_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            home = root / "home" / "operator"
            unit = home / ".config" / "systemd" / "user" / "niulang-client.service"
            unit.parent.mkdir(parents=True)
            unit.write_text(
                '[Service]\nExecStart="/home/operator/niulangd" "client" '
                '"--providers" "/home/operator/providers.json" '
                '"--local-address" "if:eth1" "--metrics-listen" "127.0.0.1:12999" '
                '"--log-level" "warn"\n',
                encoding="utf-8",
            )
            fake_bin = root / "fake-bin"
            fake_bin.mkdir()
            (fake_bin / "systemctl").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            (fake_bin / "systemctl").chmod(0o755)
            result = self.run_shell(
                "install_systemd_client_max_sessions 4096",
                root,
                {"PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            updated = unit.read_text(encoding="utf-8")
            self.assertIn('"--max-sessions" "4096"', updated)
            self.assertIn('"--local-address" "if:eth1"', updated)
            self.assertIn('"--metrics-listen" "127.0.0.1:12999"', updated)
            self.assertIn('"--log-level" "warn"', updated)

    def test_systemd_server_binary_update_recognizes_installed_unit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            unit = root / "etc" / "systemd" / "system" / "niulangd.service"
            unit.parent.mkdir(parents=True)
            unit.write_text("[Service]\n", encoding="utf-8")
            installed = root / "usr" / "local" / "bin" / "niulangd"
            installed.parent.mkdir(parents=True)
            installed.write_text("#!/bin/sh\necho 'niulangd old wire=2'\n", encoding="utf-8")
            installed.chmod(0o755)
            supplied = root / "new-niulangd"
            supplied.write_text("#!/bin/sh\necho 'niulangd new wire=2'\n", encoding="utf-8")
            supplied.chmod(0o755)

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

            result = self.run_shell(
                'run_root() { "$@"; }\nscript_dir="$PWD/deploy"\nupdate_binary_only',
                root,
                {
                    "NIULANG_BINARY": str(supplied),
                    "NIULANG_INSTALLED_BINARY": str(installed),
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                    "SYSTEMCTL_LOG": str(systemctl_log),
                    "TMPDIR": str(root),
                },
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(installed.read_text(encoding="utf-8"), supplied.read_text(encoding="utf-8"))
            self.assertEqual(systemctl_log.read_text(encoding="utf-8"), "restart niulangd.service\n")
            self.assertIn("已原子更新二进制并重启本机 Niulang 服务", result.stderr)
            self.assertNotIn("not found", result.stderr)

    def test_systemd_server_status_recognizes_installed_unit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            unit = root / "etc" / "systemd" / "system" / "niulangd.service"
            unit.parent.mkdir(parents=True)
            unit.write_text("[Service]\n", encoding="utf-8")
            fake_bin = root / "fake-bin"
            fake_bin.mkdir()
            systemctl = fake_bin / "systemctl"
            systemctl.write_text(
                "#!/bin/sh\n"
                'test "$*" = "is-active --quiet niulangd.service" || exit 1\n'
                'exit "${SYSTEMCTL_RESULT:-0}"\n',
                encoding="utf-8",
            )
            systemctl.chmod(0o755)
            environment = {"PATH": f"{fake_bin}:{os.environ['PATH']}"}

            active = self.run_shell("print_service_status server", root, environment)
            inactive = self.run_shell(
                "print_service_status server",
                root,
                {**environment, "SYSTEMCTL_RESULT": "3"},
            )

            self.assertEqual(active.returncode, 0, active.stderr)
            self.assertIn("server   active", active.stdout)
            self.assertEqual(inactive.returncode, 0, inactive.stderr)
            self.assertIn("server   inactive", inactive.stdout)
            self.assertNotIn("not found", active.stderr + inactive.stderr)

    def test_native_binary_install_and_service_definition_roll_back(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            installed = root / "usr" / "bin" / "niulangd"
            installed.parent.mkdir(parents=True)
            installed.write_text(
                "#!/bin/sh\necho 'niulangd old wire=2'\n",
                encoding="utf-8",
            )
            installed.chmod(0o755)
            supplied = root / "new-niulangd"
            supplied.write_text(
                "#!/bin/sh\necho 'niulangd new wire=2'\n",
                encoding="utf-8",
            )
            supplied.chmod(0o755)
            service = root / "etc" / "init.d" / "niulang-client"
            service.parent.mkdir(parents=True)
            service.write_text("old service\n", encoding="utf-8")
            service.chmod(0o755)
            result = self.run_shell(
                """
                run_root() { "$@"; }
                script_dir="$PWD/deploy"
                detect_init_system
                install_native_binary
                test "$("$(root_path /usr/bin/niulangd)" version)" = 'niulangd new wire=2'
                restore_native_binary
                test "$("$(root_path /usr/bin/niulangd)" version)" = 'niulangd old wire=2'
                native_start_service() { return 1; }
                native_stop_service() { return 0; }
                ! write_native_service client '--profile /etc/niulang/client.json --max-sessions 2048'
                grep -qx 'old service' "$(native_service_path client)"
                """,
                root,
                {
                    "NIULANG_INIT_SYSTEM": "openwrt",
                    "NIULANG_BINARY": str(supplied),
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
