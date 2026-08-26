#!/usr/bin/env bash
# Exercise short transfers across independent loss, burst loss, and correlated
# delay wander. This guards against calling a bulk-goodput gain a stability
# improvement when completion rate or request latency regresses.
set -Euo pipefail

usage() {
    echo "Usage: $0 OUTPUT_DIR" >&2
    echo "Optional: SING_BOX=/path/to/sing-box adds the real Hysteria2 implementation" >&2
    echo "Environment: QUEQIAO_TRIALS=5 QUEQIAO_BYTES=2097152" >&2
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

trials=${QUEQIAO_TRIALS:-5}
bytes=${QUEQIAO_BYTES:-2097152}
timeout=${QUEQIAO_TIMEOUT:-90s}
sing_box=${SING_BOX:-}
if [[ -n "$sing_box" && ! -x "$sing_box" ]]; then
    echo "SING_BOX is not executable: $sing_box" >&2
    exit 1
fi

mkdir -p "$output"
{
    echo 'format=queqiao-loss-resilience-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "go_version=$(go version)"
    echo "trials=$trials"
    echo "bytes=$bytes"
    if [[ -n "$sing_box" ]]; then echo "sing_box=$($sing_box version | head -n 1)"; fi
} >"$output/manifest.txt"
git status --porcelain=v1 --untracked-files=normal >"$output/source-status.txt"
write_source_patch "$output/source.patch"

printf 'path\tmode\tcomplete\tmedian_mbits\tworst_mbits\tmedian_cold_ms\tmedian_warm_ms\n' >"$output/summary.tsv"

run_cell() {
    local path_name=$1 mode=$2 stack=$3 controller=$4
    shift 4
    local json="$output/${path_name}-${mode}.json"
    local log="$output/${path_name}-${mode}.txt"
    local extra=("$@")
    local stack_args=(--stacks "$stack")
    if [[ "$stack" == hysteria2 ]]; then stack_args+=(--sing-box "$sing_box"); fi
    if [[ "$controller" == brutal-no-comp ]]; then extra+=(--brutal-rate 42.5); fi

    go run ./cmd/queqiaobench "${stack_args[@]}" \
        --rtt 226 --rate 50 --bytes "$bytes" --flows 1 --trials "$trials" \
        --timeout "$timeout" --latency --congestion "$controller" \
        --json "$json" "${extra[@]}" | tee "$log"

    jq -r --arg path "$path_name" --arg mode "$mode" '
        def median:
            sort | if length == 0 then 0 elif length % 2 == 1 then .[length/2|floor]
            else ((.[length/2-1] + .[length/2]) / 2) end;
        .summary[0] as $s |
        [.latency[] | select(.complete) | .cold_ms] as $cold |
        [.latency[] | select(.complete) | .warm_ms] as $warm |
        [$path, $mode,
         (($s.completed|tostring) + "/" + ($s.trials|tostring)),
         $s.median_mbits_all_trials, $s.worst_mbits_all_trials,
         ($cold | median), ($warm | median)] | @tsv
    ' "$json" >>"$output/summary.tsv"
}

for path_name in independent burst wander; do
    path_args=()
    case "$path_name" in
        independent) path_args=(--loss 20) ;;
        burst)       path_args=(--loss 20 --loss-burst 8) ;;
        wander)      path_args=(--loss 10 --delay-wander 107) ;;
    esac
    run_cell "$path_name" erasure queqiao erasure "${path_args[@]}"
    run_cell "$path_name" bbr-tuic queqiao bbr-tuic "${path_args[@]}"
    run_cell "$path_name" brutal-wire-cap queqiao brutal-no-comp "${path_args[@]}"
    if [[ -n "$sing_box" ]]; then
        run_cell "$path_name" hysteria2 hysteria2 bbr-tuic "${path_args[@]}"
    fi
done

echo "Results: $output/summary.tsv"
