#!/usr/bin/env bash
# Measure independent application flows through a local niulang SOCKS5 node.
# Callers must start and health-check the local client separately.
set -Eeuo pipefail

proxy=${NIULANG_SOCKS5:-127.0.0.1:12080}
url=${NIULANG_URL:-https://cachefly.cachefly.net/10mb.test}
expected_bytes=${NIULANG_EXPECTED_BYTES:-10485760}
trials=${NIULANG_TRIALS:-5}
timeout_seconds=${NIULANG_TIMEOUT_SECONDS:-90}
flow_list=${NIULANG_FLOWS:-"1 2 4 8"}
output=${NIULANG_OUTPUT:--}

usage() {
    cat >&2 <<'EOF'
Usage: bench_http.sh [--proxy host:port] [--url https://...] [--expected-bytes N]
                       [--trials N] [--timeout seconds] [--flows "1 2 4 8"]
                       [--output FILE]

Environment variables with NIULANG_ prefixes provide the same defaults.
Each row is one HTTP flow. Success requires curl exit 0, HTTP 200, and the
exact expected body length. Partial bodies and timeout failures stay in output.
EOF
}

while (($#)); do
    case "$1" in
        --proxy) proxy=$2; shift 2 ;;
        --url) url=$2; shift 2 ;;
        --expected-bytes) expected_bytes=$2; shift 2 ;;
        --trials) trials=$2; shift 2 ;;
        --timeout) timeout_seconds=$2; shift 2 ;;
        --flows) flow_list=$2; shift 2 ;;
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

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/niulang-bench.XXXXXX")
pids=()
cleanup() {
    local pid
    for pid in "${pids[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    for pid in "${pids[@]}"; do
        wait "$pid" 2>/dev/null || true
    done
    pids=()
    rm -rf "$tmp_dir"
}
on_signal() {
    cleanup
    exit 130
}
trap cleanup EXIT
trap on_signal INT TERM

if [[ "$output" == - ]]; then
    out_fd=/dev/stdout
else
    if [[ -e "$output" ]]; then
        echo "output already exists: $output" >&2
        exit 1
    fi
    out_fd=$output
fi
printf 'trial\tflows\tflow\thttp_code\tbytes\ttotal_seconds\tspeed_bytes_per_sec\tcurl_exit\tcomplete\n' >"$out_fd"

trial=0
for flows in $flow_list; do
    [[ "$flows" =~ ^[1-9][0-9]*$ ]] || { echo "invalid flow count: $flows" >&2; exit 2; }
    for ((trial_in_block = 1; trial_in_block <= trials; trial_in_block++)); do
        trial=$((trial + 1))
        pids=()
        for ((flow = 1; flow <= flows; flow++)); do
            result="$tmp_dir/t${trial}-f${flow}.tsv"
            (
                set +e
                curl --noproxy '' --socks5-hostname "$proxy" --max-time "$timeout_seconds" \
                    --output /dev/null --silent --show-error \
                    --write-out "%{http_code}\t%{size_download}\t%{time_total}\t%{speed_download}\t%{exitcode}\n" \
                    "$url" >"$result" 2>"$result.err"
                exit 0
            ) &
            pids+=("$!")
        done
        for pid in "${pids[@]}"; do wait "$pid" || true; done
        for ((flow = 1; flow <= flows; flow++)); do
            result="$tmp_dir/t${trial}-f${flow}.tsv"
            if IFS=$'\t' read -r code bytes seconds speed curl_exit <"$result"; then
                :
            else
                code=000; bytes=0; seconds="$timeout_seconds"; speed=0; curl_exit=1
            fi
            complete=0
            if [[ "$curl_exit" == 0 && "$code" == 200 && "$bytes" == "$expected_bytes" ]]; then complete=1; fi
            printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$trial" "$flows" "$flow" "$code" "$bytes" "$seconds" "$speed" "$curl_exit" "$complete" >>"$out_fd"
        done
    done
done
