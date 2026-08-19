# Releases, installation, and rollback

Release archives are built by `cmd/queqiaopack`, not by host-specific `tar`
or `zip` commands. The packager fixes archive ordering, timestamps, ownership,
permissions, build metadata, and Go path information. Given the same commit,
Go toolchain, version, and commit timestamp, it produces the same bytes.

## Build a release locally

Start from a clean checkout at the commit being released. Use the commit time,
not the wall clock, as the build date:

```sh
version=v0.1.0
commit=$(git rev-parse HEAD)
build_date=$(git show -s --format=%cI HEAD)
go run ./cmd/queqiaopack \
  --version "$version" --commit "$commit" --build-date "$build_date" \
  --output dist
```

The default target set is Linux, macOS, and Windows on amd64 and arm64. The
output directory must not already exist, preventing a new release from being
mixed with stale artifacts. Every target has an adjacent CycloneDX SBOM, and
each archive contains the same SBOM plus complete license texts for the exact
modules linked into its binary. `SHA256SUMS` covers every archive and SBOM.

Verify a downloaded release before unpacking it:

```sh
sha256sum -c SHA256SUMS                 # Linux
shasum -a 256 -c SHA256SUMS             # macOS
```

Then confirm that the embedded metadata matches the tag:

```sh
./queqiaod --version
cat BUILDINFO
```

Both outputs must report `wire=1` / `wire_protocol=1`. The public protocol-1 contract
accepts only protocol 1 and intentionally provides no compatibility or
downgrade path for the removed shared-secret protocol.

Validate the complete directory before executing an archive:

```sh
./scripts/validate_release.py dist
```

The validator checks checksum coverage, archive paths and modes, build metadata,
binary hashes, internal/external SBOM identity, dependency/license coverage,
and the wire version.

## Atomic Unix installation

Do not overwrite the running binary without retaining the exact prior build.
The running process is unaffected by a rename, so installation and rollback
can be prepared before a controlled service restart:

```sh
sudo install -d -m 0755 /usr/local/lib/queqiao/rollback
installed=/usr/local/bin/queqiaod
backup=/usr/local/lib/queqiao/rollback/queqiaod-$(date -u +%Y%m%dT%H%M%SZ)
test ! -e "$installed" || sudo cp -p "$installed" "$backup"
sudo install -m 0755 ./queqiaod "${installed}.new"
sudo mv "${installed}.new" "$installed"
sudo systemctl restart queqiaod
sudo systemctl is-active --quiet queqiaod
"$installed" --version
```

Keep provider state and client profiles outside the release directory.
Upgrading the binary then cannot replace identities or known working state.

For a macOS LaunchAgent, use the same `install`/`mv` sequence for
`~/.queqiao/bin/queqiaod`, then run:

```sh
launchctl kickstart -k "gui/$(id -u)/me.01.queqiao.client"
```

## Roll back

Choose the backup explicitly, verify its embedded version, install it through
a temporary pathname, and restart:

```sh
backup=/usr/local/lib/queqiao/rollback/queqiaod-YYYYMMDDTHHMMSSZ
"$backup" --version
sudo install -m 0755 "$backup" /usr/local/bin/queqiaod.rollback
sudo mv /usr/local/bin/queqiaod.rollback /usr/local/bin/queqiaod
sudo systemctl restart queqiaod
sudo systemctl is-active --quiet queqiaod
```

If a new build fails its smoke check, roll back the binary before changing the
configuration. That preserves one variable per incident. The Clash side needs
no binary rollback: select the previous profile or remove the `queqiao` SOCKS5
node and rule.

## Non-publishing release candidate

Run `.github/workflows/release-candidate.yml` manually on the exact commit under
review with a version such as `v0.1.0-rc.1`. It runs the full, race, fuzz,
vulnerability, fallback, history-secret, and reproducibility gates. The full
Go suite runs natively on Linux, macOS, and Windows for amd64 and arm64; the
race suite runs on every one of those targets supported by Go (all except
Windows/arm64). A separate six-native-runner gate executes each packaged
archive. The workflow uploads candidate archives and evidence but cannot create
a tag or GitHub Release.

GitHub artifact attestations on Free/Pro/Team require a public repository. The
candidate workflow therefore records them when the repository is public and
explicitly reports them as deferred while it is private. Publication cannot
run while the repository is private.

## Review and publishing

1. Review the candidate commit, all candidate workflow jobs, checksums, SBOMs,
   secret-scan report, native smokes, and release checklist.
2. Configure the GitHub `public-release` environment with the required human
   reviewer and prevent administrator bypass where the repository plan permits.
3. After approval, create an immutable `v*` tag on the reviewed commit. Do not
   move a tag; replace a bad candidate with a new version.
4. Manually run `.github/workflows/release.yml` with the tag, full reviewed
   commit SHA, and successful candidate workflow run ID.

The release workflow refuses a private repository, a moved/mismatched tag, or a
candidate run for another commit. It rebuilds the final version, executes the
downloaded archive on native Linux, macOS, and Windows runners for amd64 and
arm64, creates build-provenance attestations for every published file and
CycloneDX attestations for every binary, waits at the `public-release`
environment, and only then creates the GitHub Release.

Verify provenance after downloading:

```sh
gh attestation verify ./queqiaod_v0.1.0_linux_amd64.tar.gz \
  --repo bojieli/queqiao
```
