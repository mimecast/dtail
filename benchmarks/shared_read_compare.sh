#!/usr/bin/env bash
set -euo pipefail

# Compare three prebuilt checkouts serially. Separate work directories keep
# per-build references and per-round logs; no builds run during measurement.
# Usage: shared_read_compare.sh WORKDIR PREPLAN BASELINE FINAL [ROUNDS]
# Set TRACE=yes for one diagnostic round, never for timing comparisons.
declare -r SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

_die() {
    printf 'shared_read_compare: %s\n' "$*" >&2
    exit 1
}

main() {
    (($# == 4 || $# == 5)) || _die 'need workdir and three binary directories'
    local workdir=$1 preplan=$2 baseline=$3 final=$4 rounds=${5:-6}
    [[ "$rounds" =~ ^[1-9][0-9]*$ ]] || _die 'rounds must be positive'
    local -a names=(preplan latest-baseline final)
    local -a roots=("$preplan" "$baseline" "$final")
    local -i root_index
    local binary
    for root_index in 0 1 2; do
        for binary in dserver dtail; do
            [[ -x "${roots[root_index]}/$binary" ]] \
                || _die "missing binary: ${roots[root_index]}/$binary"
        done
        roots[root_index]="$(cd "${roots[root_index]}" && pwd -P)"
    done
    [[ ! -e "$workdir" && ! -L "$workdir" ]] \
        || _die 'use a fresh comparison directory'
    mkdir "$workdir"
    workdir="$(cd "$workdir" && pwd -P)"
    {
        date -u +%Y-%m-%dT%H:%M:%SZ
        uname -a
        printf 'GOMAXPROCS=%s\n' "${GOMAXPROCS:-default}"
        if command -v go > /dev/null; then go version; fi
        for root_index in 0 1 2; do
            printf '\n[%s]\nroot=%s\n' "${names[root_index]}" "${roots[root_index]}"
            git -C "${roots[root_index]}" rev-parse HEAD 2>/dev/null \
                || printf 'revision unavailable (binary-only directory)\n'
        done
    } > "$workdir/metadata.txt"
    for root_index in 0 1 2; do
        sha256sum "${roots[root_index]}/dserver" "${roots[root_index]}/dtail"
    done > "$workdir/binaries.sha256"
    local -a trace=()
    [[ ${TRACE:-no} != yes ]] || trace=(-s)
    local -i round slot index prior_rows
    local name scenario csv
    printf 'round,scenario,build\n' > "$workdir/order.csv"
    for ((round = 1; round <= rounds; round++)); do
        for scenario in follow follow-paced scheduled scheduled-gz; do
            for ((slot = 0; slot < 3; slot++)); do
                # Rotate the first build, reversing the order every other
                # round. The underlying harness also reverses sharing modes.
                if ((round % 2)); then
                    index=$(((round - 1 + slot) % 3))
                else
                    index=$(((round - 1 + 3 - slot) % 3))
                fi
                name=${names[index]}
                csv="$workdir/$name/results.csv"
                mkdir -p "$workdir/$name"
                prior_rows=0
                [[ ! -f "$csv" ]] || prior_rows=$(wc -l < "$csv")
                printf '%s,%s,%s\n' "$round" "$scenario" "$name" \
                    | tee -a "$workdir/order.csv"
                bash "$SCRIPT_DIR/shared_read_bench.sh" \
                    -d "${roots[index]}" -i "$round" -r 1 -n 4 -q 900 \
                    -w "$workdir/$name" -o "$csv" "${trace[@]}" "$scenario"
                awk -F, -v expected="$((prior_rows + 2))" \
                    'NF != 13 || $13 != "yes" { bad=1 }
                     END { exit (bad || NR != expected) }' "$csv" \
                    || _die "output mismatch in $csv"
            done
        done
    done
    # The underlying harness compares scheduled outputs within a build.
    # Check all four queries across revisions as well, for every observation.
    local -i job
    local kind mode reference candidate
    for kind in plain gz; do
        for ((job = 0; job < 4; job++)); do
            reference="$workdir/preplan/runs/scheduled-$kind-on-1/job$job.csv"
            for name in "${names[@]}"; do
                for mode in on off; do
                    for ((round = 1; round <= rounds; round++)); do
                        candidate="$workdir/$name/runs/scheduled-$kind-$mode-$round/job$job.csv"
                        cmp "$reference" "$candidate" \
                            || _die "cross-build output mismatch: $candidate"
                    done
                done
            done
        done
    done
    sha256sum -c "$workdir/binaries.sha256" \
        || _die 'binaries changed during measurement'
    printf 'All within-build and cross-build output checks passed.\n'
}

main "$@"
