#!/usr/bin/env bash
# Measure residual application UDP loss and delivered-packet latency. QUIC
# packet counters alone cannot show what FEC, retransmission, or stream HOL did
# to the datagrams the application actually received.
set -Euo pipefail

usage() {
    echo "Usage: $0 OUTPUT_DIR" >&2
    echo "Optional: SING_BOX=/path/to/sing-box adds the real Hysteria2 implementation" >&2
    echo "Environment: QUEQIAO_TRIALS=3 QUEQIAO_UDP_PACKETS=80 QUEQIAO_UDP_INTERVAL=20ms" >&2
}

[[ $# -eq 1 ]] || { usage; exit 2; }
output=$1
case "$output" in /*) ;; *) output=$PWD/$output ;; esac
[[ ! -e "$output" ]] || { echo "output directory already exists: $output" >&2; exit 1; }

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
source "$repo/scripts/benchmark_source.sh"
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

trials=${QUEQIAO_TRIALS:-3}
packets=${QUEQIAO_UDP_PACKETS:-80}
payload=${QUEQIAO_UDP_PAYLOAD:-256}
interval=${QUEQIAO_UDP_INTERVAL:-20ms}
settle=${QUEQIAO_UDP_SETTLE:-3s}
timeout=${QUEQIAO_TIMEOUT:-45s}
sing_box=${SING_BOX:-}
if [[ -n "$sing_box" && ! -x "$sing_box" ]]; then
    echo "SING_BOX is not executable: $sing_box" >&2
    exit 1
fi

mkdir -p "$output"
{
    echo 'format=queqiao-udp-delivery-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "go_version=$(go version)"
    echo "trials=$trials"
    echo "packets=$packets"
    echo "payload=$payload"
    echo "interval=$interval"
    echo "settle=$settle"
    if [[ -n "$sing_box" ]]; then echo "sing_box=$($sing_box version | head -n 1)"; fi
} >"$output/manifest.txt"
git status --porcelain=v1 --untracked-files=normal >"$output/source-status.txt"
write_source_patch "$output/source.patch"

printf 'path\tmode\tsetup\tsent\treceived\tresidual_loss_percent\tmedian_p50_ms\tmedian_p95_ms\tmedian_max_ms\tbottleneck_drops\n' >"$output/summary.tsv"

run_cell() {
    local path_name=$1 mode=$2 stack=$3 controller=$4 brutal=$5 stream=$6
    shift 6
    local json="$output/${path_name}-${mode}.json"
    local log="$output/${path_name}-${mode}.txt"
    local extra=("$@")
    local stack_args=(--stacks "$stack")
    if [[ "$stack" == hysteria2 ]]; then stack_args+=(--sing-box "$sing_box"); fi
    if [[ "$brutal" != 0 ]]; then extra+=(--brutal-rate "$brutal"); fi
    if [[ "$stream" == 1 ]]; then extra+=(--udp-on-stream); fi

    go run ./cmd/queqiaobench "${stack_args[@]}" \
        --bytes 1024 --flows 1 --trials "$trials" --timeout "$timeout" \
        --udp-packets "$packets" --udp-payload "$payload" \
        --udp-interval "$interval" --udp-settle "$settle" \
        --congestion "$controller" --json "$json" "${extra[@]}" | tee "$log"

    jq -r --arg path "$path_name" --arg mode "$mode" '
        def median:
            sort | if length == 0 then 0 elif length % 2 == 1 then .[length/2|floor]
            else ((.[length/2-1] + .[length/2]) / 2) end;
        ([.udp[] | select(.sent > 0)] | length) as $setup |
        ([.udp[].sent] | add // 0) as $sent |
        ([.udp[].received] | add // 0) as $received |
        ([.udp[] | select(.received > 0) | .p50_ms] | median) as $p50 |
        ([.udp[] | select(.received > 0) | .p95_ms] | median) as $p95 |
        ([.udp[] | select(.received > 0) | .max_ms] | median) as $max |
        ([.udp[].path_counters.downstream.bottleneck_dropped] | add // 0) as $drops |
        [$path, $mode, (($setup|tostring) + "/" + (.udp|length|tostring)),
         $sent, $received,
         (if $sent > 0 then (10000 * ($sent-$received) / $sent | round) / 100 else 100 end),
         $p50, $p95, $max, $drops] | @tsv
    ' "$json" >>"$output/summary.tsv"
}

for path_name in independent burst policer; do
    path_args=()
    fixed=47.5
    case "$path_name" in
        independent) path_args=(--rtt 226 --rate 50 --loss 15) ;;
        burst)       path_args=(--rtt 226 --rate 50 --loss 15 --loss-burst 8) ;;
        policer)     path_args=(--rtt 200 --rate 10 --loss 10 --policer-refill 8ms); fixed=9.5 ;;
    esac
    run_cell "$path_name" erasure-datagram queqiao erasure 0 0 "${path_args[@]}"
    run_cell "$path_name" erasure-stream queqiao erasure 0 1 "${path_args[@]}"
    run_cell "$path_name" fixed-wire-datagram queqiao brutal-no-comp "$fixed" 0 "${path_args[@]}"
    if [[ -n "$sing_box" ]]; then
        run_cell "$path_name" hysteria2 hysteria2 bbr-tuic 0 0 "${path_args[@]}"
    fi
done

echo "Results: $output/summary.tsv"
