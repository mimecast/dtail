#!/usr/bin/env bash
set -euo pipefail

# Test the actual harness function without sudo privileges or evicting the
# host's page cache. In particular, !/if callers must not suppress failure.
declare -r script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
declare -r _DEFAULT_LOCAL_ROOT="$(dirname "$script_dir")"
declare -r test_dir="$(mktemp -d "${TMPDIR:-/tmp}/dtail-cache-test.XXXXXXXX")"

_cleanup() {
    rm -f -- "$test_dir/cache-drops.log" "$test_dir/continued" \
        "$test_dir/preferred-helper"
    rmdir -- "$test_dir"
}
trap _cleanup EXIT

# Load only the function under test, never the build/run entrypoint. Substitute
# its preferred helper path with an isolated fixture, so both selection branches
# run regardless of whether this host has the real root-owned helper installed.
declare function_source
function_source=$(sed -n '/^_drop_caches() {$/,/^}$/p' \
    "$script_dir/upstream_vs_local_bench.sh")
function_source=${function_source//\/usr\/local\/sbin\/drop-caches/$test_dir\/preferred-helper}
source <(printf '%s\n' "$function_source")

_die() {
    printf '%s\n' "$*" >&2
    exit 1
}

sudo() {
    [[ "$1" == -n && "$2" == "$expected_helper" ]] || return 99
    printf 'simulated-cache-command status=%d\n' "$cache_status"
    return "$cache_status"
}

declare -r _WORKDIR="$test_dir"
declare -i cache_status=0

_check_helper() {
    local -r expected_helper=$1
    : > "$test_dir/cache-drops.log"
    cache_status=0
    _drop_caches
    [[ "$(grep -c '^cache-drop-ok ' "$test_dir/cache-drops.log")" == 1 ]]
    grep -q 'simulated-cache-command status=0' "$test_dir/cache-drops.log"

    cache_status=7
    if (
        # Both the subshell and this function are in conditional contexts.
        if ! _drop_caches; then
            printf 'incorrectly returned\n' > "$test_dir/continued"
        fi
        printf 'incorrectly continued\n' > "$test_dir/continued"
    ); then
        _die 'cache failure was ignored'
    fi
    [[ ! -f "$test_dir/continued" ]]
    [[ "$(grep -c '^cache-drop-ok ' "$test_dir/cache-drops.log")" == 1 ]]
    grep -q 'simulated-cache-command status=7' "$test_dir/cache-drops.log"
    printf 'cache helper selection/failure tests passed: %s\n' "$expected_helper"
}

touch "$test_dir/preferred-helper"
chmod +x "$test_dir/preferred-helper"
_check_helper "$test_dir/preferred-helper"
rm -- "$test_dir/preferred-helper"
_check_helper "$_DEFAULT_LOCAL_ROOT/benchmarks/drop_caches.sh"
printf 'cache control tests passed\n'
