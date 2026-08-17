#!/usr/bin/env bash
set -euo pipefail

parent=$(mktemp -d "${TMPDIR:-/tmp}/queqiao-credential-test.XXXXXX")
trap 'rm -rf "$parent"' EXIT HUP INT TERM
output="$parent/credentials"
./scripts/generate_credentials.sh --output "$output" --server-name test.queqiao.invalid \
  --server-days 2 --root-days 3 >/dev/null

file_mode() {
  stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"
}

openssl verify -CAfile "$output/root-ca.crt" "$output/server.crt" >/dev/null
openssl x509 -in "$output/server.crt" -noout -checkhost test.queqiao.invalid >/dev/null
test "$(wc -c <"$output/secret" | tr -d ' ')" -ge 64
test "$(file_mode "$output")" = 700
test "$(file_mode "$output/secret")" = 600
test "$(file_mode "$output/root-ca.key")" = 600
test "$(file_mode "$output/server.key")" = 600
test "$(file_mode "$output/root-ca.crt")" = 644
test "$(file_mode "$output/server.crt")" = 644
