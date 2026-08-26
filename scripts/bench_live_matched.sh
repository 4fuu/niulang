#!/usr/bin/env bash
# Alternate trials between two already-running SOCKS5 endpoints against one
# fixed remote object.
#
# The China-US path moves between roughly 0% and 50% loss within minutes, so
# running all of one transport's trials and then all of the other's compares
# two different paths. Alternating within a round, and reporting per-round
# pairs, keeps the comparison inside one path window.
set -Eeuo pipefail

a_proxy=${NIULANG_A_PROXY:-127.0.0.1:12140}
b_proxy=${NIULANG_B_PROXY:-127.0.0.1:12141}
a_label=${NIULANG_A_LABEL:-niulang}
b_label=${NIULANG_B_LABEL:-reference}
url=${NIULANG_URL:-}
expected_bytes=${NIULANG_EXPECTED_BYTES:-10485760}
rounds=${NIULANG_ROUNDS:-5}
timeout_seconds=${NIULANG_TIMEOUT_SECONDS:-120}
output=${NIULANG_OUTPUT:--}

usage() {
    cat >&2 <<'EOF'
Usage: bench_live_matched.sh --url URL [--a-proxy host:port] [--b-proxy host:port]
       [--a-label NAME] [--b-label NAME] [--expected-bytes N] [--rounds N]
       [--timeout SECONDS] [--output FILE]

Both proxies must already be running and must reach the same remote object.
A row is complete only when curl exits 0, the response is HTTP 200, and the
exact expected body length arrives.
EOF
}

while (($#)); do
    case "$1" in
        --url) url=$2; shift 2 ;;
        --a-proxy) a_proxy=$2; shift 2 ;;
        --b-proxy) b_proxy=$2; shift 2 ;;
        --a-label) a_label=$2; shift 2 ;;
        --b-label) b_label=$2; shift 2 ;;
        --expected-bytes) expected_bytes=$2; shift 2 ;;
        --rounds) rounds=$2; shift 2 ;;
        --timeout) timeout_seconds=$2; shift 2 ;;
        --output) output=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

[[ -n "$url" ]] || { echo "--url is required" >&2; usage; exit 2; }
[[ "$rounds" =~ ^[1-9][0-9]*$ ]] || { echo "rounds must be positive" >&2; exit 2; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 127; }

if [[ "$output" == - ]]; then out=/dev/stdout; else
    [[ ! -e "$output" ]] || { echo "output already exists: $output" >&2; exit 1; }
    out=$output
fi
printf 'round\torder\tlabel\thttp_code\tbytes\tseconds\tmbits_per_sec\tcurl_exit\tcomplete\n' >"$out"

trial() {
    local round=$1 order=$2 label=$3 proxy=$4
    local result code bytes seconds speed exit_code complete mbits
    result=$(curl --noproxy '' --socks5-hostname "$proxy" --max-time "$timeout_seconds" \
        --output /dev/null --silent --show-error \
        --write-out '%{http_code}\t%{size_download}\t%{time_total}\t%{speed_download}\t%{exitcode}' \
        "$url" 2>/dev/null) || true
    IFS=$'\t' read -r code bytes seconds speed exit_code <<<"$result" || true
    code=${code:-000}; bytes=${bytes:-0}; seconds=${seconds:-$timeout_seconds}
    speed=${speed:-0}; exit_code=${exit_code:-1}
    complete=0
    if [[ "$exit_code" == 0 && "$code" == 200 && "$bytes" == "$expected_bytes" ]]; then complete=1; fi
    mbits=$(awk -v b="$bytes" -v s="$seconds" 'BEGIN{if (s>0) printf "%.3f", b*8/s/1000000; else print "0"}')
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$round" "$order" "$label" "$code" "$bytes" "$seconds" "$mbits" "$exit_code" "$complete" >>"$out"
}

for ((round = 1; round <= rounds; round++)); do
    # Swap which transport goes first each round so neither systematically
    # gets the warmer or the more congested half of a round.
    if ((round % 2 == 1)); then
        trial "$round" 1 "$a_label" "$a_proxy"
        trial "$round" 2 "$b_label" "$b_proxy"
    else
        trial "$round" 1 "$b_label" "$b_proxy"
        trial "$round" 2 "$a_label" "$a_proxy"
    fi
done
