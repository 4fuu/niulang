#!/usr/bin/env python3
"""Validate a complete queqiao release directory without extracting archives."""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import pathlib
import re
import tarfile
import zipfile


REQUIRED_ARCHIVE_FILES = {
    "BUILDINFO",
    "CHANGELOG.md",
    "CONTRIBUTING.md",
    "LICENSE",
    "PRIVACY.md",
    "README.md",
    "SBOM.cdx.json",
    "SECURITY.md",
    "THIRD_PARTY_LICENSES.txt",
    "THIRD_PARTY_NOTICES.md",
    "assets/queqiao-icon.png",
    "deploy/clash-queqiao.yaml",
    "deploy/me.01.queqiao.client.plist",
    "deploy/queqiaod.service",
    "docs/ARCHITECTURE.md",
    "docs/BENCHMARKING.md",
    "docs/CONTRIBUTING-NETWORK-EVIDENCE.md",
    "docs/DEPLOYING.md",
    "docs/DESIGN.md",
    "docs/FIELD-VALIDATION.md",
    "docs/KNOWN-LIMITATIONS.md",
    "docs/LOGGING.md",
    "docs/MOBILE.md",
    "docs/PATH-CHARACTER-20260813.md",
    "docs/PRODUCTION-DESIGN.md",
    "docs/PROTOCOL.md",
    "docs/README.md",
    "docs/RELEASE-CHECKLIST.md",
    "docs/RELEASING.md",
    "docs/ROADMAP.md",
    "docs/STATUS.md",
    "docs/VISION.md",
    "docs/archive/README.md",
    "docs/archive/2026-08-development/DESIGN-ERASURE.md",
    "docs/archive/2026-08-development/DESIGN-MULTIPATH.md",
    "docs/archive/2026-08-development/MEASUREMENTS-20260809.md",
    "docs/archive/2026-08-development/MEASUREMENTS-20260810.md",
    "docs/archive/2026-08-development/MEASUREMENTS-20260816.md",
    "docs/archive/2026-08-development/PERFORMANCE-20260812.md",
    "docs/archive/2026-08-development/PROFILE-20260811.md",
    "docs/archive/2026-08-development/PUBLIC-HISTORY-AUDIT-20260817.md",
    "docs/archive/2026-08-development/README.md",
    "docs/archive/2026-08-development/RELEASE-CANDIDATE-20260817.md",
    "docs/archive/2026-08-development/RELEASE-HARDENING-20260817.md",
    "docs/archive/2026-08-development/STALL-20260817.md",
    "docs/archive/2026-08-development/STATIC-SECURITY-AUDIT-20260817.md",
    "docs/archive/2026-08-development/TCP-FALLBACK-20260817.md",
    "docs/archive/2026-08-development/field-results/20260817-primary-high-port.md",
    "docs/field-results/README.md",
    "internal/congestion/NOTICE",
}
CHECKSUM = re.compile(r"[0-9a-f]{64}")
BUILDINFO_KEYS = {
    "version",
    "commit",
    "build_date",
    "target",
    "go",
    "wire_protocol",
    "binary_sha256",
}


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def parse_checksums(path: pathlib.Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        fields = line.split("  ", 1)
        if (
            len(fields) != 2
            or CHECKSUM.fullmatch(fields[0]) is None
            or not fields[1]
            or "/" in fields[1]
            or "\\" in fields[1]
        ):
            raise ValueError(f"invalid SHA256SUMS line {number}")
        if fields[1] in result:
            raise ValueError(f"duplicate checksum entry {fields[1]}")
        result[fields[1]] = fields[0]
    return result


def archive_relative(name: str, expected_root: str) -> str:
    if "\\" in name:
        raise ValueError(f"unsafe archive member {name!r}")
    member = pathlib.PurePosixPath(name)
    parts = member.parts
    if member.is_absolute() or len(parts) < 2 or parts[0] != expected_root:
        raise ValueError(f"unsafe archive member {name!r}")
    if any(part in {"", ".", ".."} for part in parts):
        raise ValueError(f"unsafe archive member {name!r}")
    return "/".join(parts[1:])


def archive_files(path: pathlib.Path, expected_root: str) -> dict[str, tuple[bytes, int]]:
    result: dict[str, tuple[bytes, int]] = {}
    if path.suffix == ".zip":
        with zipfile.ZipFile(path) as archive:
            for entry in archive.infolist():
                if entry.is_dir():
                    raise ValueError(f"unexpected archive directory {entry.filename!r}")
                relative = archive_relative(entry.filename, expected_root)
                if relative in result:
                    raise ValueError(f"duplicate archive member {relative!r}")
                mode = (entry.external_attr >> 16) & 0o777
                result[relative] = (archive.read(entry), mode)
        return result
    with tarfile.open(path, "r:gz") as archive:
        for entry in archive.getmembers():
            if not entry.isfile():
                raise ValueError(f"unexpected non-file archive member {entry.name!r}")
            relative = archive_relative(entry.name, expected_root)
            if relative in result:
                raise ValueError(f"duplicate archive member {relative!r}")
            reader = archive.extractfile(entry)
            if reader is None:
                raise ValueError(f"cannot read archive member {entry.name!r}")
            result[relative] = (reader.read(), entry.mode & 0o777)
    return result


def properties(component: dict) -> dict[str, str]:
    return {item["name"]: item["value"] for item in component.get("properties", [])}


def parse_buildinfo(data: bytes) -> dict[str, str]:
    result: dict[str, str] = {}
    for line in data.decode("utf-8").splitlines():
        key, separator, value = line.partition("=")
        if not separator or key in result:
            raise ValueError("invalid BUILDINFO")
        result[key] = value
    if set(result) != BUILDINFO_KEYS or any(not value for value in result.values()):
        raise ValueError("BUILDINFO keys or values are incomplete")
    return result


def validate_sbom(data: bytes, archive_name: str, archive: dict[str, tuple[bytes, int]]) -> None:
    bom = json.loads(data)
    if bom.get("bomFormat") != "CycloneDX" or bom.get("specVersion") != "1.5":
        raise ValueError(f"{archive_name}: unsupported SBOM identity")
    component = bom.get("metadata", {}).get("component", {})
    if component.get("type") != "application" or component.get("name") != "queqiaod":
        raise ValueError(f"{archive_name}: invalid root SBOM component")
    if component.get("licenses") != [{"license": {"id": "MIT"}}]:
        raise ValueError(f"{archive_name}: invalid root SBOM license")
    component_properties = properties(component)
    if component_properties.get("queqiao:wire-protocol") != "1":
        raise ValueError(f"{archive_name}: SBOM does not declare wire protocol 1")
    buildinfo = parse_buildinfo(archive["BUILDINFO"][0])
    for sbom_key, build_key in (
        ("queqiao:commit", "commit"),
        ("queqiao:target", "target"),
        ("queqiao:wire-protocol", "wire_protocol"),
    ):
        if component_properties.get(sbom_key) != buildinfo.get(build_key):
            raise ValueError(f"{archive_name}: SBOM {sbom_key} disagrees with BUILDINFO")
    if component.get("version") != buildinfo.get("version"):
        raise ValueError(f"{archive_name}: SBOM version disagrees with BUILDINFO")

    binary_name = "queqiaod.exe" if buildinfo["target"].startswith("windows/") else "queqiaod"
    binary, mode = archive[binary_name]
    if mode & 0o111 == 0:
        raise ValueError(f"{archive_name}: binary is not executable")
    if sha256(binary) != buildinfo.get("binary_sha256"):
        raise ValueError(f"{archive_name}: binary hash disagrees with BUILDINFO")
    hashes = {item["alg"]: item["content"] for item in component.get("hashes", [])}
    if hashes.get("SHA-256") != buildinfo["binary_sha256"]:
        raise ValueError(f"{archive_name}: binary hash disagrees with SBOM")

    module_components = bom.get("components", [])
    modules = {item["name"] for item in module_components}
    component_refs = {item["bom-ref"] for item in module_components}
    if not modules or len(modules) != len(module_components) or len(component_refs) != len(module_components):
        raise ValueError(f"{archive_name}: SBOM has no linked modules")
    if any(
        not item.get("licenses")
        or not item["licenses"][0].get("license", {}).get("id")
        for item in module_components
    ):
        raise ValueError(f"{archive_name}: an SBOM module has no SPDX license")
    license_bundle = archive["THIRD_PARTY_LICENSES.txt"][0].decode("utf-8")
    missing_licenses = sorted(module for module in modules if module not in license_bundle)
    if missing_licenses:
        raise ValueError(f"{archive_name}: license bundle omits {missing_licenses}")
    root_ref = component.get("bom-ref")
    dependencies = {item["ref"]: set(item.get("dependsOn", [])) for item in bom.get("dependencies", [])}
    if dependencies.get(root_ref) != component_refs:
        raise ValueError(f"{archive_name}: SBOM dependency graph is incomplete")


def validate_release(directory: pathlib.Path) -> None:
    checksums_path = directory / "SHA256SUMS"
    if not checksums_path.is_file():
        raise ValueError("SHA256SUMS is missing")
    expected = parse_checksums(checksums_path)
    actual_names = {path.name for path in directory.iterdir() if path.is_file() and path.name != "SHA256SUMS"}
    if actual_names != set(expected):
        raise ValueError(f"checksum coverage differs: files={sorted(actual_names)} sums={sorted(expected)}")
    for name, digest in expected.items():
        if sha256((directory / name).read_bytes()) != digest:
            raise ValueError(f"checksum mismatch for {name}")

    sboms = sorted(directory.glob("*.cdx.json"))
    if not sboms:
        raise ValueError("no CycloneDX SBOMs found")
    for sbom_path in sboms:
        stem = sbom_path.name.removesuffix(".cdx.json")
        archive_path = directory / (stem + (".zip" if stem.endswith(("windows_amd64", "windows_arm64")) else ".tar.gz"))
        if not archive_path.is_file():
            raise ValueError(f"archive for {sbom_path.name} is missing")
        contents = archive_files(archive_path, stem)
        binary_name = "queqiaod.exe" if archive_path.suffix == ".zip" else "queqiaod"
        required = REQUIRED_ARCHIVE_FILES | {binary_name}
        if set(contents) != required:
            raise ValueError(
                f"{archive_path.name}: archive coverage differs: "
                f"missing={sorted(required - set(contents))} "
                f"unexpected={sorted(set(contents) - required)}"
            )
        for name, (_, mode) in contents.items():
            expected_mode = 0o755 if name == binary_name else 0o644
            if mode != expected_mode:
                raise ValueError(
                    f"{archive_path.name}: {name} mode is {mode:o}, want {expected_mode:o}"
                )
        external_sbom = sbom_path.read_bytes()
        if contents["SBOM.cdx.json"][0] != external_sbom:
            raise ValueError(f"{archive_path.name}: internal and external SBOM differ")
        validate_sbom(external_sbom, archive_path.name, contents)
        buildinfo = parse_buildinfo(contents["BUILDINFO"][0])
        expected_stem = "queqiaod_{}_{}".format(
            buildinfo["version"], buildinfo["target"].replace("/", "_")
        )
        if stem != expected_stem:
            raise ValueError(f"{archive_path.name}: filename disagrees with BUILDINFO")

    archives = list(directory.glob("*.tar.gz")) + list(directory.glob("*.zip"))
    if len(archives) != len(sboms):
        raise ValueError(f"archive/SBOM count differs: {len(archives)} != {len(sboms)}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("directory", type=pathlib.Path)
    args = parser.parse_args()
    validate_release(args.directory.resolve())
    print(f"validated release artifacts in {args.directory}")


if __name__ == "__main__":
    main()
