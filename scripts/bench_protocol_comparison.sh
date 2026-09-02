#!/usr/bin/env bash
# Compare the current Niulang data plane with released Queqiao and the real
# Hysteria2 and AnyTLS implementations from sing-box. Clean-path four-stack
# cells are observations across two different relay models; lossy cells contain
# only the three UDP/QUIC stacks because the userspace TCP relay cannot emulate
# segment loss honestly.
set -Euo pipefail

usage() {
    cat >&2 <<'EOF'
Usage: SING_BOX=/path/to/sing-box QUEQIAO=/path/to/queqiaod \
       bench_protocol_comparison.sh OUTPUT_DIR

Environment:
  NIULANG_TRIALS=5             ordinary cells
  NIULANG_IMPORTANT_TRIALS=7   long-haul 1% and 5% cells
  NIULANG_TIMEOUT=90s          per-trial timeout
  NIULANG_SEED=8201            deterministic path seed
  TOOL_PROVENANCE_DIR=DIR      optional release metadata copied into the bundle
EOF
}

[[ $# -eq 1 ]] || { usage; exit 2; }
output=$1
case "$output" in /*) ;; *) output=$PWD/$output ;; esac
[[ ! -e "$output" ]] || { echo "output directory already exists: $output" >&2; exit 1; }

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
source "$repo/scripts/benchmark_source.sh"

sing_box=${SING_BOX:-}
queqiao=${QUEQIAO:-}
trials=${NIULANG_TRIALS:-5}
important_trials=${NIULANG_IMPORTANT_TRIALS:-7}
timeout=${NIULANG_TIMEOUT:-90s}
seed=${NIULANG_SEED:-8201}
provenance=${TOOL_PROVENANCE_DIR:-}

[[ -x "$sing_box" ]] || { echo "SING_BOX must name an executable" >&2; exit 1; }
[[ -x "$queqiao" ]] || { echo "QUEQIAO must name an executable" >&2; exit 1; }
[[ "$trials" =~ ^[1-9][0-9]*$ ]] || { echo "NIULANG_TRIALS must be positive" >&2; exit 2; }
[[ "$important_trials" =~ ^[1-9][0-9]*$ ]] || { echo "NIULANG_IMPORTANT_TRIALS must be positive" >&2; exit 2; }
[[ "$seed" =~ ^[0-9]+$ ]] || { echo "NIULANG_SEED must be a non-negative integer" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
time_bin=$(command -v /usr/bin/time || true)
[[ -n "$time_bin" ]] || { echo "GNU /usr/bin/time is required" >&2; exit 1; }

mkdir -p "$output/json" "$output/logs" "$output/tools"
if [[ -n "$provenance" ]]; then
    [[ -d "$provenance" ]] || { echo "TOOL_PROVENANCE_DIR is not a directory" >&2; exit 1; }
    cp -a "$provenance"/. "$output/tools"/
fi

source_status=$(git status --porcelain=v1 --untracked-files=normal)
tree_state=clean
[[ -z "$source_status" ]] || tree_state=modified
{
    echo 'format=niulang-protocol-comparison-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "tree_state=$tree_state"
    echo "go_version=$(go version)"
    echo "goos=$(go env GOOS)"
    echo "goarch=$(go env GOARCH)"
    echo "kernel=$(uname -srmo)"
    echo "cpu_count=$(getconf _NPROCESSORS_ONLN)"
    echo "cpu_model=$(lscpu | sed -n 's/^Model name:[[:space:]]*//p' | head -n 1)"
    echo "memory_bytes=$(awk '/^MemTotal:/ {printf "%.0f", $2 * 1024}' /proc/meminfo)"
    echo "ordinary_trials=$trials"
    echo "important_trials=$important_trials"
    echo "timeout=$timeout"
    echo "seed=$seed"
    echo "sing_box_version=$($sing_box version | head -n 1)"
    echo "sing_box_sha256=$(sha256sum "$sing_box" | awk '{print $1}')"
    echo "queqiao_version=$($queqiao --version)"
    echo "queqiao_sha256=$(sha256sum "$queqiao" | awk '{print $1}')"
    echo 'clean_four_stack_scope=delay/rate/backpressure observations; UDP and TCP relays are not directly interchangeable'
    echo 'lossy_scope=matched UDP packet emulator; AnyTLS excluded because userspace TCP cannot emulate segment loss'
} >"$output/manifest.txt"
if [[ -n "$source_status" ]]; then printf '%s\n' "$source_status" >"$output/source-status.txt"
else : >"$output/source-status.txt"
fi
write_source_patch "$output/source.patch"

build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
bench="$build_dir/niulangbench"
go build -trimpath -o "$bench" ./cmd/niulangbench

printf 'family\tprofile\tstacks\ttrials\tstatus\tintegrity\n' >"$output/cells.tsv"
printf 'family\tprofile\tstacks\ttrials\twall_seconds\tuser_seconds\tsystem_seconds\tcpu_percent\tmax_rss_kib\texit_code\n' >"$output/resources.tsv"

all_stacks=niulang,hysteria2,anytls,queqiao
quic_stacks=niulang,hysteria2,queqiao
failures=0

run_cell() {
    local family=$1 profile=$2 stacks=$3 cell_trials=$4
    shift 4
    local json="$output/json/${family}-${profile}.json"
    local log="$output/logs/${family}-${profile}.txt"
    local status=0 integrity=ok
    local command=("$bench"
        --stacks "$stacks"
        --sing-box "$sing_box"
        --queqiao "$queqiao"
        --congestion erasure
        --trials "$cell_trials"
        --timeout "$timeout"
        --seed "$seed"
        --json "$json"
        "$@")

    printf '[%s] %s/%s (%s trials)\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$family" "$profile" "$cell_trials"
    {
        printf '# command='
        printf '%q ' "${command[@]}"
        printf '\n'
    } >"$log"
    if "$time_bin" -a -o "$output/resources.tsv" \
        -f "$family\t$profile\t$stacks\t$cell_trials\t%e\t%U\t%S\t%P\t%M\t%x" \
        "${command[@]}" >>"$log" 2>&1; then
        status=0
    else
        status=$?
        failures=$((failures + 1))
    fi
    if [[ ! -s "$json" ]]; then
        integrity=no-json
        failures=$((failures + 1))
    elif ! jq -e '
        ([.trials[]?.note, .latency[]?.note, .udp[]?.note]
         | map(select(type == "string" and startswith("setup:")))
         | length) == 0
    ' "$json" >/dev/null; then
        integrity=setup-failure
        failures=$((failures + 1))
    fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$family" "$profile" "$stacks" "$cell_trials" "$status" "$integrity" >>"$output/cells.tsv"
}

# Bulk useful goodput. Four-stack clean cells are retained as observations,
# while loss, jitter and wander are matched only among the QUIC stacks.
run_cell bulk clean-edge "$all_stacks" "$trials" --rtt 50 --rate 100 --loss 0 --bytes $((8*1024*1024))
run_cell bulk clean-longhaul "$all_stacks" "$important_trials" --rtt 226 --rate 100 --loss 0 --bytes $((16*1024*1024))
run_cell bulk clean-longhaul-25m "$all_stacks" "$trials" --rtt 226 --rate 25 --loss 0 --bytes $((8*1024*1024))
run_cell bulk clean-high-rtt "$all_stacks" "$trials" --rtt 400 --rate 50 --loss 0 --bytes $((8*1024*1024))
run_cell bulk local-datapath "$all_stacks" "$trials" --rtt 0 --rate 0 --loss 0 --bytes $((64*1024*1024))
run_cell bulk clean-concurrent-8 "$all_stacks" "$trials" --rtt 226 --rate 100 --loss 0 --bytes $((4*1024*1024)) --flows 8

run_cell bulk loss1-mid "$quic_stacks" "$trials" --rtt 100 --rate 100 --loss 1 --bytes $((8*1024*1024))
run_cell bulk loss1-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 1 --bytes $((16*1024*1024))
run_cell bulk loss5-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 5 --bytes $((8*1024*1024))
run_cell bulk loss5-burst4 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 5 --loss-burst 4 --bytes $((8*1024*1024))
run_cell bulk loss10-live "$quic_stacks" "$trials" --rtt 264 --rate 50 --loss 10 --bytes $((4*1024*1024))
run_cell bulk loss20-live "$quic_stacks" "$trials" --rtt 264 --rate 50 --loss 20 --bytes $((4*1024*1024))
run_cell bulk asymmetric-up10 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 1 --loss-up 10 --bytes $((8*1024*1024))
run_cell bulk loss1-jitter20 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 1 --jitter 20 --bytes $((8*1024*1024))
run_cell bulk loss1-wander107 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 1 --delay-wander 107 --bytes $((8*1024*1024))

# Short-lived cold and warm requests.
run_cell latency clean-edge "$all_stacks" "$important_trials" --rtt 50 --rate 100 --loss 0 --bytes $((64*1024)) --latency
run_cell latency clean-longhaul "$all_stacks" "$important_trials" --rtt 226 --rate 100 --loss 0 --bytes $((64*1024)) --latency
run_cell latency clean-high-rtt "$all_stacks" "$trials" --rtt 400 --rate 50 --loss 0 --bytes $((64*1024)) --latency
run_cell latency loss1-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 1 --bytes $((64*1024)) --latency
run_cell latency loss5-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 5 --bytes $((64*1024)) --latency
run_cell latency loss5-burst4 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 5 --loss-burst 4 --bytes $((64*1024)) --latency
run_cell latency loss1-high-rtt "$quic_stacks" "$trials" --rtt 400 --rate 50 --loss 1 --bytes $((64*1024)) --latency

# Small-request tail while bulk occupies the path.
run_cell interactive clean-edge "$all_stacks" "$trials" --rtt 50 --rate 100 --loss 0 --bytes $((8*1024*1024)) --interactive
run_cell interactive clean-longhaul "$all_stacks" "$important_trials" --rtt 226 --rate 100 --loss 0 --bytes $((16*1024*1024)) --interactive
run_cell interactive clean-high-rtt "$all_stacks" "$trials" --rtt 400 --rate 50 --loss 0 --bytes $((8*1024*1024)) --interactive
run_cell interactive loss1-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 1 --bytes $((16*1024*1024)) --interactive
run_cell interactive loss5-longhaul "$quic_stacks" "$important_trials" --rtt 226 --rate 100 --loss 5 --bytes $((8*1024*1024)) --interactive
run_cell interactive loss5-burst4 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 5 --loss-burst 4 --bytes $((8*1024*1024)) --interactive

# Mixed storefront fanout and UDP delivery/latency supplement the three core
# families with representative multi-flow and datagram workloads.
run_cell page clean-edge "$all_stacks" "$trials" --rtt 50 --rate 50 --loss 0 --page
run_cell page clean-longhaul "$all_stacks" "$trials" --rtt 226 --rate 50 --loss 0 --page
run_cell page loss1-longhaul "$quic_stacks" "$trials" --rtt 226 --rate 50 --loss 1 --page
run_cell page loss5-mid "$quic_stacks" "$trials" --rtt 100 --rate 50 --loss 5 --page

run_cell udp clean-edge "$all_stacks" "$trials" --rtt 50 --rate 100 --loss 0 --bytes 1024 --udp-packets 100 --udp-payload 1200 --udp-interval 10ms --udp-settle 2s
run_cell udp clean-longhaul "$all_stacks" "$trials" --rtt 226 --rate 100 --loss 0 --bytes 1024 --udp-packets 100 --udp-payload 1200 --udp-interval 10ms --udp-settle 2s
run_cell udp loss1-longhaul "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 1 --bytes 1024 --udp-packets 100 --udp-payload 1200 --udp-interval 10ms --udp-settle 2s
run_cell udp loss5-longhaul "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 5 --bytes 1024 --udp-packets 100 --udp-payload 1200 --udp-interval 10ms --udp-settle 2s
run_cell udp loss5-burst4 "$quic_stacks" "$trials" --rtt 226 --rate 100 --loss 5 --loss-burst 4 --bytes 1024 --udp-packets 100 --udp-payload 1200 --udp-interval 10ms --udp-settle 2s

echo "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$output/manifest.txt"
(
    cd "$output"
    find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS
)
echo "Results: $output"
if ((failures)); then
    echo "bench_protocol_comparison: $failures execution or setup failure(s)" >&2
    exit 1
fi
