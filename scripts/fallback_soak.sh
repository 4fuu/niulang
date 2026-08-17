#!/usr/bin/env bash
# Repeatedly exercise UDP block, TCP rescue, source-preserving resume, and UDP
# recovery. Results carry source/toolchain provenance so a passing soak can be
# attached to a release rather than copied out of a terminal.
set -Euo pipefail

runs=${QUEQIAO_SOAK_RUNS:-20}
race_runs=${QUEQIAO_SOAK_RACE_RUNS:-5}
output_dir=${QUEQIAO_SOAK_OUTPUT:-}
invocation=("$0" "$@")

usage() {
    cat >&2 <<'EOF'
Usage: fallback_soak.sh --output-dir DIR [--runs N] [--race-runs N]

The output directory must not exist. Normal and race logs, source status, any
tracked source patch, a manifest, and SHA-256 checksums are retained there.
EOF
}

while (($#)); do
    case "$1" in
        --output-dir) output_dir=$2; shift 2 ;;
        --runs) runs=$2; shift 2 ;;
        --race-runs) race_runs=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

[[ -n "$output_dir" ]] || { echo "--output-dir is required" >&2; usage; exit 2; }
[[ "$runs" =~ ^[1-9][0-9]*$ ]] || { echo "runs must be positive" >&2; exit 2; }
[[ "$race_runs" =~ ^[1-9][0-9]*$ ]] || { echo "race runs must be positive" >&2; exit 2; }
[[ ! -e "$output_dir" ]] || { echo "output directory already exists: $output_dir" >&2; exit 1; }

source_status=$(git status --porcelain=v1 --untracked-files=normal)
tree_state=clean
if [[ -n "$source_status" ]]; then tree_state=modified; fi
mkdir -p "$output_dir"
{
    echo 'format=queqiao-fallback-soak-v1'
    echo "started_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "commit=$(git rev-parse HEAD)"
    echo "tree_state=$tree_state"
    echo "go_version=$(go version)"
    echo "goos=$(go env GOOS)"
    echo "goarch=$(go env GOARCH)"
    echo "runs=$runs"
    echo "race_runs=$race_runs"
    printf 'command='
    printf '%q ' "${invocation[@]}"
    printf '\n'
} >"$output_dir/manifest.txt"
if [[ -n "$source_status" ]]; then printf '%s\n' "$source_status" >"$output_dir/source-status.txt"
else : >"$output_dir/source-status.txt"
fi
git diff --binary HEAD >"$output_dir/source.patch"

tests='TestUDPAssociationRescuesToTCP|TestARescuedUDPAssociationKeepsItsRemoteSourceAddress|TestIntermittentUDPBlockingReturnsToQUIC'
status=0
go test ./internal/pep -run "^(${tests})$" -count "$runs" -timeout 45m \
    2>&1 | tee "$output_dir/normal.log" || status=$?
go test -race ./internal/pep -run "^(${tests})$" -count "$race_runs" -timeout 45m \
    2>&1 | tee "$output_dir/race.log" || status=$?

(
    cd "$output_dir"
    shasum -a 256 manifest.txt source-status.txt source.patch normal.log race.log >SHA256SUMS
)
exit "$status"
