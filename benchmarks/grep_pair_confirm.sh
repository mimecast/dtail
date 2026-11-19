#!/usr/bin/env bash
set -euo pipefail

# Repeat an outlying full-suite grep result with both cache policies.
# Usage: grep_pair_confirm.sh BEFORE AFTER DATA WORKDIR [REGEX] [ROUNDS]
# Writes real output and verifies it against grep; no profiling runs overlap.
declare -r SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

_die() {
    printf 'grep_pair_confirm: %s\n' "$*" >&2
    exit 1
}

main() {
    (($# >= 4 && $# <= 6)) || _die 'need before, after, data and fresh workdir'
    local before=$1 after=$2 data=$3 workdir=$4 pattern=${5:-user999 } rounds=${6:-10}
    [[ "$rounds" =~ ^[1-9][0-9]*$ ]] || _die 'rounds must be positive'
    [[ -f "$data" ]] || _die "missing input: $data"
    [[ -x "$before/dgrep" && -x "$after/dgrep" ]] || _die 'build dgrep first'
    [[ -x /usr/local/sbin/drop-caches ]] || _die 'need privileged cache helper'
    [[ ! -e "$workdir" ]] || _die 'use a fresh workdir'
    mkdir -p "$workdir"
    local status=0
    grep -E -- "$pattern" "$data" > "$workdir/expected" || status=$?
    ((status <= 1)) || _die 'invalid oracle pattern'
    printf 'cache,build,round,elapsed,user,sys,rss,rc\n' > "$workdir/results.csv"
    local policy build root metrics rc
    local -i round
    local -a order
    for policy in cold warm; do
        for ((round = 1; round <= rounds; round++)); do
            if ((round % 2)); then order=(before after); else order=(after before); fi
            for build in "${order[@]}"; do
                root=$before
                [[ "$build" != after ]] || root=$after
                if [[ "$policy" == cold ]]; then
                    sudo -n /usr/local/sbin/drop-caches >> "$workdir/cache-drops.log" 2>&1 \
                        || _die 'cache drop failed'
                    printf 'cache-drop-ok\n' >> "$workdir/cache-drops.log"
                fi
                metrics=$(python3 "$SCRIPT_DIR/measure_command.py" \
                    --stdout "$workdir/output" --stderr "$workdir/$policy-$build-$round.err" -- \
                    timeout --kill-after=5 60 "$root/dgrep" \
                    --cfg none --plain --noColor --logger stdout --logLevel error \
                    --regex "$pattern" --files "$data")
                rc=${metrics##*$'\t'}
                [[ "$rc" == 0 ]] || _die "command failed: $policy $build $round (rc=$rc)"
                cmp "$workdir/expected" "$workdir/output" || _die 'output mismatch'
                printf '%s,%s,%s,%s\n' "$policy" "$build" "$round" \
                    "${metrics//$'\t'/,}" | tee -a "$workdir/results.csv"
            done
        done
    done
    sha256sum "$workdir/expected" "$workdir/output" > "$workdir/output-sha256.txt"
}

main "$@"
