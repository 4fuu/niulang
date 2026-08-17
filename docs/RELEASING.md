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
mixed with stale artifacts. `SHA256SUMS` covers every archive.

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

Keep configuration, certificates, and the session secret outside the release
directory. Upgrading the binary then cannot replace credentials or the known
working configuration.

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

## Publishing

Pushing a `v*` tag starts `.github/workflows/release.yml`.
The workflow derives the build date from the tagged commit, runs tests, builds
all six archives, verifies their checksums, and attaches them to a GitHub
Release. Tags are immutable release inputs; replace a bad release with a new
version rather than moving its tag.
