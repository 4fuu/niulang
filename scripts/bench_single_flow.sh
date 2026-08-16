#!/usr/bin/env bash
# Measure one application flow per trial. The client configuration (especially
# transport settings) are selected by the caller before this script is
# run; the output is intentionally one row per logical SOCKS connection, not
# several independent downloads disguised as one result.
set -Eeuo pipefail

proxy=${QUEQIAO_SOCKS5:-127.0.0.1:12080}
url=${QUEQIAO_URL:-https://cachefly.cachefly.net/10mb.test}
expected_bytes=${QUEQIAO_EXPECTED_BYTES:-10485760}
trials=${QUEQIAO_TRIALS:-5}
timeout_seconds=${QUEQIAO_TIMEOUT_SECONDS:-90}
output=${QUEQIAO_OUTPUT:--}
label=${QUEQIAO_LABEL:-single-flow}

usage() {
    cat >&2 <<'EOF'
Usage: bench_single_flow.sh [--proxy host:port] [--url https://...] \
       [--expected-bytes N] [--trials N] [--timeout seconds] \
       [--label NAME] [--output FILE]

The SOCKS client must already be running. A result is complete only when curl
exits successfully, returns HTTP 200, and receives the exact body length.
EOF
}

while (($#)); do
    case "$1" in
        --proxy) proxy=$2; shift 2 ;;
        --url) url=$2; shift 2 ;;
        --expected-bytes) expected_bytes=$2; shift 2 ;;
        --trials) trials=$2; shift 2 ;;
        --timeout) timeout_seconds=$2; shift 2 ;;
        --label) label=$2; shift 2 ;;
        --output) output=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

[[ "$proxy" == *:* ]] || { echo "proxy must be host:port" >&2; exit 2; }
[[ "$url" == https://* || "$url" == http://* ]] || { echo "url must be HTTP(S)" >&2; exit 2; }
[[ "$expected_bytes" =~ ^[0-9]+$ && "$expected_bytes" -gt 0 ]] || { echo "expected-bytes must be positive" >&2; exit 2; }
[[ "$trials" =~ ^[1-9][0-9]*$ ]] || { echo "trials must be positive" >&2; exit 2; }
[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || { echo "timeout must be positive" >&2; exit 2; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 127; }

if [[ "$output" == - ]]; then
    out_fd=/dev/stdout
else
    [[ ! -e "$output" ]] || { echo "output already exists: $output" >&2; exit 1; }
    out_fd=$output
fi
printf 'trial\tlabel\thttp_code\tbytes\ttotal_seconds\tspeed_bytes_per_sec\tcurl_exit\tcomplete\n' >"$out_fd"

for ((trial = 1; trial <= trials; trial++)); do
    result=$(mktemp "${TMPDIR:-/tmp}/queqiao-single.XXXXXX")
    errfile="$result.err"
    trap 'rm -f "$result" "$errfile"' EXIT INT TERM
    set +e
    curl --noproxy '' --socks5-hostname "$proxy" --max-time "$timeout_seconds" \
        --output /dev/null --silent --show-error \
        --write-out "%{http_code}\t%{size_download}\t%{time_total}\t%{speed_download}\t%{exitcode}\n" \
        "$url" >"$result" 2>"$errfile"
    set -e
    if IFS=$'\t' read -r code bytes seconds speed curl_exit <"$result"; then :; else
        code=000; bytes=0; seconds=$timeout_seconds; speed=0; curl_exit=1
    fi
    complete=0
    if [[ "$curl_exit" == 0 && "$code" == 200 && "$bytes" == "$expected_bytes" ]]; then complete=1; fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$trial" "$label" "$code" "$bytes" "$seconds" "$speed" "$curl_exit" "$complete" >>"$out_fd"
    rm -f "$result" "$errfile"
    trap - EXIT INT TERM
done
