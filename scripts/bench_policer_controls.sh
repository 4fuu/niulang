#!/usr/bin/env bash
# Compare Queqiao's current controller with fixed-rate and aggregate-budget
# controls on a token-bucket policer. The important outputs are useful goodput,
# interactive p95, and bottleneck_drop_percent: optimizing only one of them can
# make the other two worse.
set -Euo pipefail

usage() {
    echo "Usage: $0 OUTPUT_DIR" >&2
    echo "Environment: QUEQIAO_TRIALS=3 QUEQIAO_RATE_MBITS=10 QUEQIAO_LOSS='0 20' QUEQIAO_BYTES=4194304" >&2
}

[[ $# -eq 1 ]] || { usage; exit 2; }
output=$1
case "$output" in /*) ;; *) output=$PWD/$output ;; esac
[[ ! -e "$output" ]] || { echo "output directory already exists: $output" >&2; exit 1; }

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

trials=${QUEQIAO_TRIALS:-3}
rate=${QUEQIAO_RATE_MBITS:-10}
losses=${QUEQIAO_LOSS:-"0 20"}
bytes=${QUEQIAO_BYTES:-4194304}
timeout=${QUEQIAO_TIMEOUT:-90s}
refill=${QUEQIAO_POLICER_REFILL:-8ms}

mkdir -p "$output"
{
    echo 'format=queqiao-policer-controls-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "go_version=$(go version)"
    echo "trials=$trials"
    echo "rate_mbits=$rate"
    echo "losses=$losses"
    echo "bytes=$bytes"
    echo "policer_refill=$refill"
} >"$output/manifest.txt"
git status --porcelain=v1 --untracked-files=normal >"$output/source-status.txt"
git diff --binary HEAD >"$output/source.patch"

printf 'mode\tloss_percent\tcomplete\tmedian_mbits\tinteractive_p95_ms\tdownstream_packets\tbottleneck_drops\tbottleneck_drop_percent\terased_packets\n' >"$output/summary.tsv"

run_cell() {
    local label=$1 loss=$2 controller=$3 brutal=$4 aggregate=$5 reserve=$6
    local json="$output/${label}-loss-${loss}.json"
    local log="$output/${label}-loss-${loss}.txt"
    local extra=()
    if [[ "$brutal" != 0 ]]; then extra+=(--brutal-rate "$brutal"); fi
    if [[ "$aggregate" != 0 ]]; then extra+=(--aggregate-rate "$aggregate"); fi
    if [[ "$reserve" != 0 ]]; then extra+=(--interactive-reserve "$reserve"); fi

    go run ./cmd/queqiaobench \
        --stacks queqiao --rtt 200 --rate "$rate" --loss "$loss" \
        --policer-refill "$refill" --bytes "$bytes" --flows 1 \
        --trials "$trials" --timeout "$timeout" --interactive \
        --congestion "$controller" --json "$json" "${extra[@]}" | tee "$log"

    jq -r --arg mode "$label" '
        .summary[0] as $s |
        ([.trials[].path_counters.downstream.packets_in] | add // 0) as $packets |
        ([.trials[].path_counters.downstream.bottleneck_dropped] | add // 0) as $drops |
        ([.trials[].path_counters.downstream.packets_erased] | add // 0) as $erased |
        [$mode, .path.loss_percent,
         (($s.completed|tostring) + "/" + ($s.trials|tostring)),
         $s.median_mbits_all_trials, ($s.interactive_median.p95_ms // 0),
         $packets, $drops,
         (if $packets > 0 then (10000 * $drops / $packets | round) / 100 else 0 end),
         $erased] | @tsv
    ' "$json" >>"$output/summary.tsv"
}

# Keep fixed wire pacing just below the configured policer. The aggregate
# budget is lower because it counts application frames, not QUIC/FEC wire
# bytes; it is a useful control, not a hard wire cap.
fixed=$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.95 }')
aggregate=$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.75 }')
reserve=$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.10 }')

for loss in $losses; do
    run_cell erasure-current "$loss" erasure 0 0 0
    run_cell erasure-budget "$loss" erasure 0 "$aggregate" "$reserve"
    run_cell brutal-compensating "$loss" brutal "$fixed" 0 0
    run_cell brutal-wire-cap "$loss" brutal-no-comp "$fixed" 0 0
done

echo "Results: $output/summary.tsv"
