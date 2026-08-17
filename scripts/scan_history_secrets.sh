#!/bin/sh

# Scan every reachable Git commit with a pinned, checksum-verified Gitleaks
# binary. A generated high-entropy canary must be detected first, preventing a
# broken ruleset from turning a green result into false assurance.

set -eu

version=8.24.3
repository=${1:-.}
report=${QUEQIAO_SECRET_SCAN_REPORT:-history-secret-scan.json}

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    asset=gitleaks_8.24.3_linux_x64.tar.gz
    expected=9991e0b2903da4c8f6122b5c3186448b927a5da4deef1fe45271c3793f4ee29c
    ;;
  Linux-aarch64|Linux-arm64)
    asset=gitleaks_8.24.3_linux_arm64.tar.gz
    expected=5f2edbe1f49f7b920f9e06e90759947d3c5dfc16f752fb93aaafc17e9d14cf07
    ;;
  Darwin-x86_64)
    asset=gitleaks_8.24.3_darwin_x64.tar.gz
    expected=41c44ae8ad1d6eef57d4526ad0fd67d8129eee9a856f55c2b3b9395fd3d9ec0f
    ;;
  Darwin-arm64)
    asset=gitleaks_8.24.3_darwin_arm64.tar.gz
    expected=b90f13bb8c90ab72083d9b0c842e39dafb82c0e5c3f872f407366b7a58909013
    ;;
  *)
    echo "unsupported scanner host $(uname -s)/$(uname -m)" >&2
    exit 2
    ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-secret-scan.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

url="https://github.com/gitleaks/gitleaks/releases/download/v${version}/${asset}"
curl -fsSL --retry 3 --connect-timeout 15 --max-time 180 -o "$work/$asset" "$url"
actual=$(shasum -a 256 "$work/$asset" | awk '{print $1}')
if [ "$actual" != "$expected" ]; then
  echo "Gitleaks checksum mismatch: got $actual, want $expected" >&2
  exit 1
fi
tar -xzf "$work/$asset" -C "$work" gitleaks

canary=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 36)
printf 'token = ghp_%s\n' "$canary" >"$work/canary.txt"
set +e
"$work/gitleaks" dir "$work/canary.txt" --no-banner >/dev/null 2>&1
canary_result=$?
set -e
if [ "$canary_result" -ne 1 ]; then
  echo "Gitleaks canary failed: scanner exit $canary_result, want finding exit 1" >&2
  exit 1
fi
rm "$work/canary.txt"

"$work/gitleaks" git "$repository" --redact --no-banner \
  --report-format json --report-path "$report"
echo "Gitleaks $version scanned all reachable history; report: $report"
