#!/usr/bin/env bash
# Run the low-latency, relatively high-bandwidth, low-loss campaign. Cells are
# deliberately serial: concurrent benchmark processes would make CPU, memory,
# completion, and tail-latency comparisons depend on each other's workload.
set -Euo pipefail

usage() {
    cat >&2 <<'EOF'
Usage: bench_low_latency_bandwidth.sh OUTPUT_DIR

Environment:
  SING_BOX=/path/to/sing-box       add real Hysteria2 cells
  QUEQIAO_TRIALS=5                 ordinary cell trials
  QUEQIAO_IMPORTANT_TRIALS=10      key cell trials
  QUEQIAO_FAMILIES='throughput policer udp reuse'
  QUEQIAO_PROFILE_SET=full|smoke   full is the default
  QUEQIAO_TIMEOUT=90s              per-trial timeout

The full profile concentrates on 0%, 1%, and 5% independent loss. One 15%
cell is retained only to make UDP-on-stream head-of-line delay measurable.
EOF
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
time_bin=$(command -v /usr/bin/time || true)
[[ -n "$time_bin" ]] || { echo "GNU /usr/bin/time is required" >&2; exit 1; }

trials=${QUEQIAO_TRIALS:-5}
important_trials=${QUEQIAO_IMPORTANT_TRIALS:-10}
families=${QUEQIAO_FAMILIES:-"throughput policer udp reuse"}
profile_set=${QUEQIAO_PROFILE_SET:-full}
timeout=${QUEQIAO_TIMEOUT:-90s}
sing_box=${SING_BOX:-}

[[ "$trials" =~ ^[1-9][0-9]*$ ]] || { echo "QUEQIAO_TRIALS must be positive" >&2; exit 2; }
[[ "$important_trials" =~ ^[1-9][0-9]*$ ]] || { echo "QUEQIAO_IMPORTANT_TRIALS must be positive" >&2; exit 2; }
case "$profile_set" in full|smoke) ;; *) echo "QUEQIAO_PROFILE_SET must be full or smoke" >&2; exit 2 ;; esac
if [[ -n "$sing_box" && ! -x "$sing_box" ]]; then
    echo "SING_BOX is not executable: $sing_box" >&2
    exit 1
fi
for family in $families; do
    case "$family" in throughput|policer|udp|reuse) ;; *) echo "unknown family: $family" >&2; exit 2 ;; esac
done

mkdir -p "$output/json" "$output/logs"
source_status=$(git status --porcelain=v1 --untracked-files=normal)
tree_state=clean
[[ -z "$source_status" ]] || tree_state=modified
{
    echo 'format=queqiao-low-latency-bandwidth-v1'
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
    echo "profile_set=$profile_set"
    echo "families=$families"
    echo "trials=$trials"
    echo "important_trials=$important_trials"
    echo "timeout=$timeout"
    if [[ -n "$sing_box" ]]; then
        echo "sing_box_version=$($sing_box version | head -n 1)"
        echo "sing_box_sha256=$(sha256sum "$sing_box" | awk '{print $1}')"
    else
        echo 'sing_box_version=not-configured'
    fi
} >"$output/manifest.txt"
if [[ -n "$source_status" ]]; then printf '%s\n' "$source_status" >"$output/source-status.txt"
else : >"$output/source-status.txt"
fi
write_source_patch "$output/source.patch"

build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
bench="$build_dir/queqiaobench"
go build -trimpath -o "$bench" ./cmd/queqiaobench

printf 'family\tprofile\tmode\ttrials\twall_seconds\tuser_seconds\tsystem_seconds\tcpu_percent\tmax_rss_kib\texit_code\n' >"$output/resources.tsv"
printf 'profile\tmode\trtt_ms\trate_mbits\tloss_percent\tloss_burst\ttrials\tcompleted\tsetup_failures\tmedian_mbits\tworst_mbits\tinteractive_p95_ms\tpackets_in\terased\tbottleneck_drops\tbottleneck_drop_percent\n' >"$output/throughput.tsv"
printf 'profile\tmode\trtt_ms\trate_mbits\tloss_percent\trefill_ms\ttrials\tcompleted\tmedian_mbits\tinteractive_p95_ms\tpackets_in\terased\tbottleneck_drops\tbottleneck_drop_percent\n' >"$output/policer.tsv"
printf 'profile\tmode\trtt_ms\trate_mbits\tloss_percent\tloss_burst\tpayload_bytes\ttrials\tsetup\tsent\treceived\tdelivery_percent\tresidual_loss_percent\tmedian_p50_ms\tmedian_p95_ms\tmedian_max_ms\terased\tbottleneck_drops\n' >"$output/udp.tsv"
printf 'profile\tmode\trtt_ms\tloss_percent\ttrials\tcompleted\tmedian_cold_ms\tmedian_warm_ms\n' >"$output/reuse.tsv"

run_timed() {
    local family=$1 profile=$2 mode=$3 cell_trials=$4 log=$5
    shift 5
    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $family/$profile/$mode ($cell_trials trials)"
    "$time_bin" -a -o "$output/resources.tsv" \
        -f "$family\t$profile\t$mode\t$cell_trials\t%e\t%U\t%S\t%P\t%M\t%x" \
        "$bench" "$@" >"$log" 2>&1
}

run_throughput_cell() {
    local profile=$1 rtt=$2 rate=$3 loss=$4 burst=$5 bytes=$6 cell_trials=$7 mode=$8
    local json="$output/json/throughput-${profile}-${mode}.json"
    local log="$output/logs/throughput-${profile}-${mode}.txt"
    local stack=queqiao controller=$mode
    local extra=()
    case "$mode" in
        erasure|bbr-tuic) ;;
        erasure-wire-cap)
            controller=erasure
            extra+=(--wire-cap-rate "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.95 }')"
                --wire-interactive-reserve "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.10 }')")
            ;;
        brutal-no-comp)
            extra+=(--brutal-rate "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.95 }')")
            ;;
        hysteria2)
            [[ -n "$sing_box" ]] || return 0
            stack=hysteria2
            controller=bbr-tuic
            extra+=(--sing-box "$sing_box")
            ;;
    esac
    if awk -v burst="$burst" 'BEGIN { exit !(burst > 0) }'; then extra+=(--loss-burst "$burst"); fi
    run_timed throughput "$profile" "$mode" "$cell_trials" "$log" \
        --stacks "$stack" --rtt "$rtt" --rate "$rate" --loss "$loss" \
        --bytes "$bytes" --flows 1 --trials "$cell_trials" --timeout "$timeout" \
        --congestion "$controller" --interactive --json "$json" "${extra[@]}"
    jq -r --arg profile "$profile" --arg mode "$mode" '
        def r3: (. * 1000 | round) / 1000;
        .summary[0] as $s |
        ([.trials[].path_counters.downstream.packets_in] | add // 0) as $packets |
        ([.trials[].path_counters.downstream.packets_erased] | add // 0) as $erased |
        ([.trials[].path_counters.downstream.bottleneck_dropped] | add // 0) as $drops |
        [$profile, $mode, .path.rtt_ms, .path.rate_mbits, .path.loss_percent,
         (.path.loss_burst_packets // 0), $s.trials, $s.completed, ($s.setup_failures // 0),
         ($s.median_mbits_all_trials | r3), ($s.worst_mbits_all_trials | r3),
         (($s.interactive_median.p95_ms // 0) | r3), $packets, $erased, $drops,
         (if $packets > 0 then (100000 * $drops / $packets | round) / 1000 else 0 end)] | @tsv
    ' "$json" >>"$output/throughput.tsv"
}

run_throughput() {
    local profiles
    if [[ "$profile_set" == smoke ]]; then
        profiles=$'smoke-100\t100\t100\t1\t0\t1048576\t1'
    else
        profiles=$'clean-50\t50\t100\t0\t0\t16777216\tordinary\nlow-loss-100\t100\t100\t1\t0\t16777216\tordinary\nclean-226\t226\t100\t0\t0\t33554432\tordinary\nlow-loss-226\t226\t100\t1\t0\t33554432\timportant\nupper-low-loss-226\t226\t100\t5\t0\t33554432\timportant\nburst-low-loss-226\t226\t100\t5\t4\t16777216\tordinary\nhigh-rtt-400\t400\t50\t1\t0\t16777216\tordinary'
    fi
    while IFS=$'\t' read -r profile rtt rate loss burst bytes importance; do
        local cell_trials=$trials
        [[ "$profile_set" != smoke ]] || cell_trials=1
        [[ "$importance" != important ]] || cell_trials=$important_trials
        for mode in erasure erasure-wire-cap bbr-tuic brutal-no-comp hysteria2; do
            run_throughput_cell "$profile" "$rtt" "$rate" "$loss" "$burst" "$bytes" "$cell_trials" "$mode"
        done
    done <<<"$profiles"
}

run_policer_cell() {
    local profile=$1 refill=$2 mode=$3 cell_trials=$4
    local rtt=226 rate=100 loss=1 bytes=33554432
    local json="$output/json/policer-${profile}-${mode}.json"
    local log="$output/logs/policer-${profile}-${mode}.txt"
    local controller=$mode
    local extra=()
    case "$mode" in
        erasure) ;;
        erasure-wire-cap)
            controller=erasure
            extra+=(--wire-cap-rate 95 --wire-interactive-reserve 10)
            ;;
        erasure-budget)
            controller=erasure
            extra+=(--aggregate-rate 75 --interactive-reserve 10)
            ;;
        brutal)
            extra+=(--brutal-rate 95)
            ;;
        brutal-no-comp)
            extra+=(--brutal-rate 95)
            ;;
    esac
    run_timed policer "$profile" "$mode" "$cell_trials" "$log" \
        --stacks queqiao --rtt "$rtt" --rate "$rate" --loss "$loss" \
        --policer-refill "$refill" --bytes "$bytes" --flows 1 \
        --trials "$cell_trials" --timeout "$timeout" --interactive \
        --congestion "$controller" --json "$json" "${extra[@]}"
    jq -r --arg profile "$profile" --arg mode "$mode" '
        def r3: (. * 1000 | round) / 1000;
        .summary[0] as $s |
        ([.trials[].path_counters.downstream.packets_in] | add // 0) as $packets |
        ([.trials[].path_counters.downstream.packets_erased] | add // 0) as $erased |
        ([.trials[].path_counters.downstream.bottleneck_dropped] | add // 0) as $drops |
        [$profile, $mode, .path.rtt_ms, .path.rate_mbits, .path.loss_percent,
         .path.policer_refill_ms, $s.trials, $s.completed,
         ($s.median_mbits_all_trials | r3), (($s.interactive_median.p95_ms // 0) | r3),
         $packets, $erased, $drops,
         (if $packets > 0 then (100000 * $drops / $packets | round) / 1000 else 0 end)] | @tsv
    ' "$json" >>"$output/policer.tsv"
}

run_policer() {
    local refills='1ms 8ms 16ms'
    local cell_trials=$important_trials
    if [[ "$profile_set" == smoke ]]; then refills=8ms; cell_trials=1; fi
    for refill in $refills; do
        profile=refill-${refill%ms}ms
        for mode in erasure erasure-wire-cap erasure-budget brutal brutal-no-comp; do
            run_policer_cell "$profile" "$refill" "$mode" "$cell_trials"
        done
    done
}

run_udp_cell() {
    local profile=$1 rtt=$2 rate=$3 loss=$4 burst=$5 payload=$6 cell_trials=$7 mode=$8
    local json="$output/json/udp-${profile}-${mode}.json"
    local log="$output/logs/udp-${profile}-${mode}.txt"
    local stack=queqiao controller=erasure
    local extra=()
    case "$mode" in
        erasure-datagram) ;;
        erasure-wire-cap-datagram)
            extra+=(--wire-cap-rate "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.95 }')"
                --wire-interactive-reserve "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.10 }')")
            ;;
        bbr-tuic-datagram) controller=bbr-tuic ;;
        fixed-wire-datagram)
            controller=brutal-no-comp
            extra+=(--brutal-rate "$(awk -v rate="$rate" 'BEGIN { printf "%.3f", rate * 0.95 }')")
            ;;
        erasure-stream) extra+=(--udp-on-stream) ;;
        hysteria2)
            [[ -n "$sing_box" ]] || return 0
            stack=hysteria2
            controller=bbr-tuic
            extra+=(--sing-box "$sing_box")
            ;;
    esac
    if awk -v burst="$burst" 'BEGIN { exit !(burst > 0) }'; then extra+=(--loss-burst "$burst"); fi
    run_timed udp "$profile" "$mode" "$cell_trials" "$log" \
        --stacks "$stack" --rtt "$rtt" --rate "$rate" --loss "$loss" \
        --bytes 1024 --flows 1 --trials "$cell_trials" --timeout "$timeout" \
        --udp-packets 100 --udp-payload "$payload" --udp-interval 10ms --udp-settle 3s \
        --congestion "$controller" --json "$json" "${extra[@]}"
    jq -r --arg profile "$profile" --arg mode "$mode" '
        def median: sort | if length == 0 then 0 elif length % 2 == 1 then .[length/2|floor] else ((.[length/2-1] + .[length/2]) / 2) end;
        def r3: (. * 1000 | round) / 1000;
        ([.udp[] | select(.sent > 0)] | length) as $setup |
        ([.udp[].sent] | add // 0) as $sent |
        ([.udp[].received] | add // 0) as $received |
        ([.udp[] | select(.received > 0) | .p50_ms] | median) as $p50 |
        ([.udp[] | select(.received > 0) | .p95_ms] | median) as $p95 |
        ([.udp[] | select(.received > 0) | .max_ms] | median) as $max |
        ([.udp[].path_counters.downstream.packets_erased] | add // 0) as $erased |
        ([.udp[].path_counters.downstream.bottleneck_dropped] | add // 0) as $drops |
        [$profile, $mode, .path.rtt_ms, .path.rate_mbits, .path.loss_percent,
         (.path.loss_burst_packets // 0), (.arguments[.arguments | index("--udp-payload") + 1] | tonumber),
         (.udp | length), $setup, $sent, $received,
         (if $sent > 0 then (100000 * $received / $sent | round) / 1000 else 0 end),
         (if $sent > 0 then (100000 * ($sent-$received) / $sent | round) / 1000 else 100 end),
         ($p50 | r3), ($p95 | r3), ($max | r3), $erased, $drops] | @tsv
    ' "$json" >>"$output/udp.tsv"
}

run_udp() {
    local profiles
    if [[ "$profile_set" == smoke ]]; then
        profiles=$'smoke-100\t100\t100\t1\t0\t256\t1\tall'
    else
        profiles=$'clean-50-small\t50\t100\t0\t0\t256\tordinary\tall\nlow-loss-100-mtu\t100\t100\t1\t0\t1200\tordinary\tall\nlow-loss-226-small\t226\t100\t1\t0\t256\timportant\tall\nlow-loss-226-mtu\t226\t100\t1\t0\t1200\timportant\tall\nupper-low-loss-226-mtu\t226\t100\t5\t0\t1200\timportant\tall\nburst-low-loss-226-mtu\t226\t100\t5\t4\t1200\tordinary\tall\nhigh-rtt-400-mtu\t400\t50\t1\t0\t1200\tordinary\tall\nhol-boundary-100-mtu\t100\t100\t15\t0\t1200\timportant\thol'
    fi
    while IFS=$'\t' read -r profile rtt rate loss burst payload importance scope; do
        local cell_trials=$trials
        [[ "$profile_set" != smoke ]] || cell_trials=1
        [[ "$importance" != important ]] || cell_trials=$important_trials
        local modes='erasure-datagram erasure-wire-cap-datagram bbr-tuic-datagram fixed-wire-datagram erasure-stream hysteria2'
        [[ "$scope" != hol ]] || modes='erasure-datagram erasure-stream'
        for mode in $modes; do
            run_udp_cell "$profile" "$rtt" "$rate" "$loss" "$burst" "$payload" "$cell_trials" "$mode"
        done
    done <<<"$profiles"
}

run_reuse_cell() {
    local profile=$1 rtt=$2 loss=$3 cell_trials=$4 mode=$5 pool=$6
    local json="$output/json/reuse-${profile}-${mode}.json"
    local log="$output/logs/reuse-${profile}-${mode}.txt"
    run_timed reuse "$profile" "$mode" "$cell_trials" "$log" \
        --stacks queqiao --rtt "$rtt" --rate 100 --loss "$loss" \
        --bytes 1024 --flows 1 --trials "$cell_trials" --timeout "$timeout" \
        --congestion erasure --quic-pool="$pool" --latency --json "$json"
    jq -r --arg profile "$profile" --arg mode "$mode" '
        def median: sort | if length == 0 then 0 elif length % 2 == 1 then .[length/2|floor] else ((.[length/2-1] + .[length/2]) / 2) end;
        def r3: (. * 1000 | round) / 1000;
        [.latency[] | select(.complete)] as $complete |
        [$profile, $mode, .path.rtt_ms, .path.loss_percent, (.latency | length),
         ($complete | length), ([$complete[].cold_ms] | median | r3), ([$complete[].warm_ms] | median | r3)] | @tsv
    ' "$json" >>"$output/reuse.tsv"
}

run_reuse() {
    local profiles
    local cell_trials=$important_trials
    if [[ "$profile_set" == smoke ]]; then
        profiles=$'smoke-100\t100\t1'
        cell_trials=1
    else
        profiles=$'clean-50\t50\t0\nlow-loss-100\t100\t1\nlow-loss-226\t226\t1\nupper-low-loss-226\t226\t5\nhigh-rtt-400\t400\t1'
    fi
    while IFS=$'\t' read -r profile rtt loss; do
        run_reuse_cell "$profile" "$rtt" "$loss" "$cell_trials" pooled true
        run_reuse_cell "$profile" "$rtt" "$loss" "$cell_trials" unpooled false
    done <<<"$profiles"
}

for family in $families; do
    "run_$family"
done

echo "finished_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$output/manifest.txt"
(
    cd "$output"
    find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS
)
echo "Results: $output"
