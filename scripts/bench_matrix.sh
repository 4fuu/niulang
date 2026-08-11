#!/usr/bin/env bash
# Run the emulated-path comparison matrix between wanopt and the TUIC-shaped
# reference proxy. Every row is produced by cmd/wanoptbench in one process
# against a seeded path emulator, so a difference between the two stacks in the
# same block is attributable to the transports rather than to a path window.
#
# This is the fast inner loop. It does not replace a live China-US campaign;
# see docs/MEASUREMENTS-*.md for those.
set -Eeuo pipefail

output=${WANOPT_OUTPUT:-}
trials=${WANOPT_TRIALS:-5}
congestion=${WANOPT_CONGESTION:-bbr-tuic}
timeout_seconds=${WANOPT_TIMEOUT:-180s}

usage() {
    cat >&2 <<'EOF'
Usage: bench_matrix.sh [--output FILE] [--trials N] [--congestion NAME]

Blocks measured:
  A  200 ms / 100 Mbit/s / 0,1,3,5% loss   10 MiB   1 flow    goodput parity
  B  200 ms / 100 Mbit/s / 1% loss         50 MiB   1 flow    steady state
  C  200 ms / 100 Mbit/s / 1% loss         10 MiB   4,8 flows concurrency
  D  264 ms /  50 Mbit/s / 10,20% loss     10 MiB   1 flow    live-path regime
  E  0 ms   / unlimited  / 0% loss        256 MiB   1 flow    datapath cost
  F  200 ms / 100 Mbit/s / 1% loss          1 KiB   1 flow    request latency
EOF
}

while (($#)); do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --trials) trials=$2; shift 2 ;;
        --congestion) congestion=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

[[ "$trials" =~ ^[1-9][0-9]*$ ]] || { echo "trials must be positive" >&2; exit 2; }
if [[ -n "$output" ]]; then
    [[ ! -e "$output" ]] || { echo "output already exists: $output" >&2; exit 1; }
    exec >"$output"
fi

bench() {
    go run ./cmd/wanoptbench --trials "$trials" --congestion "$congestion" \
        --timeout "$timeout_seconds" "$@"
}

echo "## block A: single-flow goodput across loss"
for loss in 0 1 3 5; do
    bench --rtt 200 --rate 100 --loss "$loss" --bytes $((10 * 1024 * 1024)) --flows 1
done

echo
echo "## block B: single-flow steady state"
bench --rtt 200 --rate 100 --loss 1 --bytes $((50 * 1024 * 1024)) --flows 1

echo
echo "## block C: concurrent flows"
bench --rtt 200 --rate 100 --loss 1 --bytes $((10 * 1024 * 1024)) --flows 4,8

echo
echo "## block D: live-path loss regime"
for loss in 10 20; do
    bench --rtt 264 --rate 50 --loss "$loss" --bytes $((10 * 1024 * 1024)) --flows 1
done

echo
echo "## block E: datapath cost with no path impairment"
bench --rtt 0 --rate 0 --loss 0 --bytes $((256 * 1024 * 1024)) --flows 1 --congestion reno

echo
echo "## block F: request latency"
bench --rtt 200 --rate 100 --loss 1 --bytes 1024 --flows 1 --latency
