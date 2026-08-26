#!/usr/bin/env bash
# Measure the cold and warm request cost of Niulang's persistent QUIC pool.
# This tests the useful low-latency idea behind AnyTLS's idle-session pool
# without importing TCP multiplexing or padding, neither of which is a latency
# mechanism.
set -Euo pipefail

usage() {
    echo "Usage: $0 OUTPUT_DIR" >&2
    echo "Environment: NIULANG_TRIALS=5 NIULANG_RTTS='50 200' NIULANG_LOSSES='0 5'" >&2
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

trials=${NIULANG_TRIALS:-5}
rtts=${NIULANG_RTTS:-"50 200"}
losses=${NIULANG_LOSSES:-"0 5"}
timeout=${NIULANG_TIMEOUT:-60s}

mkdir -p "$output"
{
    echo 'format=niulang-connection-reuse-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "go_version=$(go version)"
    echo "trials=$trials"
    echo "rtts=$rtts"
    echo "losses=$losses"
} >"$output/manifest.txt"
git status --porcelain=v1 --untracked-files=normal >"$output/source-status.txt"
write_source_patch "$output/source.patch"

printf 'mode\trtt_ms\tloss_percent\ttrial\tcold_ms\twarm_ms\tcomplete\tnote\n' >"$output/latency.tsv"

for rtt in $rtts; do
    for loss in $losses; do
        for pool in true false; do
            mode=pooled
            [[ "$pool" == true ]] || mode=unpooled
            json="$output/${mode}-rtt-${rtt}-loss-${loss}.json"
            log="$output/${mode}-rtt-${rtt}-loss-${loss}.txt"
            go run ./cmd/niulangbench \
                --stacks niulang --rtt "$rtt" --rate 100 --loss "$loss" \
                --bytes 1024 --flows 1 --trials "$trials" --timeout "$timeout" \
                --congestion erasure --quic-pool="$pool" --latency --json "$json" | tee "$log"
            jq -r --arg mode "$mode" --argjson rtt "$rtt" --argjson loss "$loss" '.latency[] |
                [$mode, $rtt, $loss, .trial, .cold_ms, .warm_ms, .complete, (.note // "")] | @tsv
            ' "$json" >>"$output/latency.tsv"
        done
    done
done

echo "Results: $output/latency.tsv"
