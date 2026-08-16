#!/usr/bin/env bash
# Run the emulated-path comparison matrix between queqiao and the TUIC-shaped
# reference proxy. Every row is produced by cmd/queqiaobench in one process
# against a seeded path emulator, so a difference between the two stacks in the
# same block is attributable to the transports rather than to a path window.
#
# This is the fast inner loop. It does not replace a live China-US campaign;
# see docs/MEASUREMENTS-*.md for those.
set -Euo pipefail

output=${QUEQIAO_OUTPUT:-}
trials=${QUEQIAO_TRIALS:-5}
congestion=${QUEQIAO_CONGESTION:-bbr-tuic}
timeout_seconds=${QUEQIAO_TIMEOUT:-180s}
gate=${QUEQIAO_GATE:-0}
tolerance=${QUEQIAO_TOLERANCE:-0.10}
json_dir=${QUEQIAO_JSON_DIR:-}

usage() {
    cat >&2 <<'EOF'
Usage: bench_matrix.sh [--output FILE] [--trials N] [--congestion NAME]
                       [--gate] [--tolerance FRACTION] [--json-dir DIR]

--gate makes each block fail when queqiao falls behind the reference by more
than --tolerance, or completes fewer transfers, so a transport regression is
rejected rather than merely printed. --json-dir writes one machine-readable
record per block, which is what makes results comparable across commits.

Blocks measured:
  A  200 ms / 100 Mbit/s / 0,1,3,5% loss   10 MiB   1 flow    goodput parity
  B  200 ms / 100 Mbit/s / 1% loss         50 MiB   1 flow    steady state
  C  200 ms / 100 Mbit/s / 1% loss         10 MiB   4,8 flows concurrency
  D  264 ms /  50 Mbit/s / 10,20% loss     10 MiB   1 flow    live-path regime
  E  0 ms   / unlimited  / 0% loss        256 MiB   1 flow    datapath cost
  F  200 ms / 100 Mbit/s / 1% loss          1 KiB   1 flow    request latency
  G  200 ms / 100 Mbit/s / 1% loss         50 MiB   1 flow    interactive tail
EOF
}

while (($#)); do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --trials) trials=$2; shift 2 ;;
        --congestion) congestion=$2; shift 2 ;;
        --gate) gate=1; shift ;;
        --tolerance) tolerance=$2; shift 2 ;;
        --json-dir) json_dir=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

[[ "$trials" =~ ^[1-9][0-9]*$ ]] || { echo "trials must be positive" >&2; exit 2; }
if [[ -n "$output" ]]; then
    [[ ! -e "$output" ]] || { echo "output already exists: $output" >&2; exit 1; }
    exec >"$output"
fi

block=0
failures=0

bench() {
    block=$((block + 1))
    local extra=()
    if [[ -n "$json_dir" ]]; then
        mkdir -p "$json_dir"
        extra+=(--json "$json_dir/block-$block.json")
    fi
    if ((gate)); then
        extra+=(--gate --tolerance "$tolerance")
    fi
    if ! go run ./cmd/queqiaobench --trials "$trials" --congestion "$congestion" \
        --timeout "$timeout_seconds" "${extra[@]}" "$@"; then
        failures=$((failures + 1))
    fi
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

echo
echo "## block G: interactive latency during a bulk transfer"
bench --rtt 200 --rate 100 --loss 1 --bytes $((50 * 1024 * 1024)) --flows 1 --interactive

if ((failures)); then
    echo >&2 "bench_matrix: $failures block(s) failed the gate"
    exit 1
fi
