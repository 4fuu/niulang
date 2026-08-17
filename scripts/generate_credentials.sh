#!/usr/bin/env bash

# Generate a private ECDSA root, a bounded-lifetime server certificate, and a
# high-entropy printable session secret. Nothing is written unless the output
# directory is new.

set -euo pipefail
umask 077

output=
server_name=queqiao.node
server_days=397
root_days=3650

usage() {
  cat >&2 <<'EOF'
Usage: generate_credentials.sh --output DIR [--server-name NAME]
                               [--server-days N] [--root-days N]

DIR must not exist. Keep root-ca.key offline; deploy server.crt/server.key to
the egress and root-ca.crt/secret to the client.
EOF
}

while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --server-name) server_name=$2; shift 2 ;;
    --server-days) server_days=$2; shift 2 ;;
    --root-days) root_days=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

[[ -n "$output" ]] || { echo "--output is required" >&2; exit 2; }
[[ ! -e "$output" ]] || { echo "output already exists: $output" >&2; exit 1; }
[[ "$server_name" =~ ^[A-Za-z0-9.-]+$ ]] || { echo "invalid server name" >&2; exit 2; }
[[ "$server_days" =~ ^[1-9][0-9]*$ ]] || { echo "server days must be positive" >&2; exit 2; }
[[ "$root_days" =~ ^[1-9][0-9]*$ ]] || { echo "root days must be positive" >&2; exit 2; }

mkdir -m 0700 "$output"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
  -out "$output/root-ca.key" 2>/dev/null
openssl req -new -x509 -sha256 -key "$output/root-ca.key" \
  -out "$output/root-ca.crt" -days "$root_days" \
  -subj "/CN=queqiao private root" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
  -out "$output/server.key" 2>/dev/null
openssl req -new -sha256 -key "$output/server.key" \
  -out "$output/server.csr" -subj "/CN=$server_name" 2>/dev/null
printf '%s\n' \
  "basicConstraints=critical,CA:FALSE" \
  "keyUsage=critical,digitalSignature" \
  "extendedKeyUsage=serverAuth" \
  "subjectAltName=DNS:$server_name" >"$output/server.ext"
serial=$(openssl rand -hex 16)
openssl x509 -req -sha256 -in "$output/server.csr" \
  -CA "$output/root-ca.crt" -CAkey "$output/root-ca.key" \
  -set_serial "0x$serial" -days "$server_days" \
  -extfile "$output/server.ext" -out "$output/server.crt" 2>/dev/null

# Printable encoding prevents bytes.TrimSpace from shortening a raw 32-byte
# secret when its first or last random byte happens to be whitespace.
openssl rand -base64 48 >"$output/secret"
chmod 0600 "$output/root-ca.key" "$output/server.key" "$output/secret"
chmod 0644 "$output/root-ca.crt" "$output/server.crt"
rm "$output/server.csr" "$output/server.ext"

openssl verify -CAfile "$output/root-ca.crt" "$output/server.crt" >/dev/null
openssl x509 -in "$output/server.crt" -noout -checkhost "$server_name" >/dev/null
echo "generated credentials in $output; keep root-ca.key offline"
