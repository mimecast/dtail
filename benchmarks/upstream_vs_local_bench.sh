#!/usr/bin/env bash

set -euo pipefail

readonly _SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly _DEFAULT_LOCAL_ROOT="$(dirname "$_SCRIPT_DIR")"
readonly _MEASURE_HELPER="$_SCRIPT_DIR/measure_command.py"
readonly _SUMMARY_HELPER="$_SCRIPT_DIR/summarize_upstream_comparison.py"
readonly _COUNT_QUERY='from STATS select count($line) group by $hostname'
readonly _AGGREGATE_QUERY='from STATS select count($line),avg($goroutines),max($goroutines),sum($goroutines) group by $goroutines'

_usage() {
    cat <<'EOF'
Usage:
  upstream_vs_local_bench.sh smoke [options]
  upstream_vs_local_bench.sh run [options]

Modes:
  smoke  Build both revisions and run tiny untimed correctness checks.
  run    Run the paired benchmark and write raw and summarized results.

Options:
  --local-root PATH       Local fork checkout (default: repository root).
  --upstream-root PATH    Upstream checkout (default: ../dtail-upstream).
  --workdir PATH          Preserve all binaries, data, logs, and results here.
  --iterations N          Use N observations for every scenario.
  --build-parallelism N   Go build parallelism (default: 1).
  -h, --help              Show this help.

The full run deliberately requires an explicit `run` mode. The smoke mode does
not collect benchmark timings or generate the 100 MiB and 1 GiB data sets.
EOF
}

_die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

_require_command() {
    command -v "$1" >/dev/null 2>&1 || _die "required command not found: $1"
}

_positive_integer() {
    [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

_drop_caches() {
    # Prefer the root-owned, sudoers-whitelisted helper; fall back to the
    # in-repo script for hosts where the invoking user may sudo it directly.
    # Explicit failure is needed even when our caller is in an if/! condition
    # (where Bash disables errexit throughout the function call chain).
    local helper="$_DEFAULT_LOCAL_ROOT/benchmarks/drop_caches.sh"
    if [[ -x /usr/local/sbin/drop-caches ]]; then
        helper=/usr/local/sbin/drop-caches
    fi
    sudo -n "$helper" >> "$_WORKDIR/cache-drops.log" 2>&1 \
        || _die "cache drop failed; see $_WORKDIR/cache-drops.log"
    printf 'cache-drop-ok %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        "$helper" >> "$_WORKDIR/cache-drops.log" \
        || _die "unable to record cache-drop receipt"
}

[[ $# -gt 0 ]] || {
    _usage
    exit 2
}

_MODE=$1
shift
case "$_MODE" in
smoke | run) ;;
-h | --help)
    _usage
    exit 0
    ;;
*)
    _usage >&2
    _die "unknown mode: $_MODE"
    ;;
esac

_LOCAL_ROOT=$_DEFAULT_LOCAL_ROOT
_UPSTREAM_ROOT="$(dirname "$_DEFAULT_LOCAL_ROOT")/dtail-upstream"
_WORKDIR=
_ITERATIONS=
_BUILD_PARALLELISM=1
while [[ $# -gt 0 ]]; do
    case "$1" in
    --local-root)
        [[ $# -ge 2 ]] || _die "--local-root requires a path"
        _LOCAL_ROOT=$2
        shift 2
        ;;
    --upstream-root)
        [[ $# -ge 2 ]] || _die "--upstream-root requires a path"
        _UPSTREAM_ROOT=$2
        shift 2
        ;;
    --workdir)
        [[ $# -ge 2 ]] || _die "--workdir requires a path"
        _WORKDIR=$2
        shift 2
        ;;
    --iterations)
        [[ $# -ge 2 ]] || _die "--iterations requires a value"
        _ITERATIONS=$2
        shift 2
        ;;
    --build-parallelism)
        [[ $# -ge 2 ]] || _die "--build-parallelism requires a value"
        _BUILD_PARALLELISM=$2
        shift 2
        ;;
    -h | --help)
        _usage
        exit 0
        ;;
    *) _die "unknown option: $1" ;;
    esac
done

_positive_integer "$_BUILD_PARALLELISM" || _die "build parallelism must be positive"
if [[ -n "$_ITERATIONS" ]]; then
    _positive_integer "$_ITERATIONS" || _die "iterations must be positive"
fi

# Cache sudo credentials to avoid repeated password prompts during cache dropping
sudo -v

for required in awk cat cmp cp date diff env getconf git go grep hostname id mkdir mktemp mv python3 sed sha256sum sleep sort ssh-keygen stat timeout tr uname wc; do
    _require_command "$required"
done

[[ -d "$_LOCAL_ROOT" ]] || _die "local checkout not found: $_LOCAL_ROOT"
[[ -d "$_UPSTREAM_ROOT" ]] || _die "upstream checkout not found: $_UPSTREAM_ROOT"
_LOCAL_ROOT="$(cd "$_LOCAL_ROOT" && pwd -P)"
_UPSTREAM_ROOT="$(cd "$_UPSTREAM_ROOT" && pwd -P)"

for root in "$_LOCAL_ROOT" "$_UPSTREAM_ROOT"; do
    [[ -f "$root/go.mod" ]] || _die "not a Go checkout: $root"
    grep -q '^module github.com/mimecast/dtail$' "$root/go.mod" ||
        _die "unexpected module path in $root/go.mod"
done

if [[ -z "$_WORKDIR" ]]; then
    _WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/dtail-upstream-comparison.XXXXXX")"
else
    mkdir -p "$_WORKDIR"
    _WORKDIR="$(cd "$_WORKDIR" && pwd -P)"
fi

readonly _BIN_DIR="$_WORKDIR/bin"
readonly _DATA_DIR="$_WORKDIR/data"
readonly _OUTPUT_DIR="$_WORKDIR/output"
readonly _SERVER_DIR="$_WORKDIR/server"
readonly _RESULTS_CSV="$_WORKDIR/results.csv"
readonly _METADATA_FILE="$_WORKDIR/metadata.txt"
readonly _AUTH_KEY="$_SERVER_DIR/auth_key"
readonly _CLOCK_TICKS="$(getconf CLK_TCK)"
mkdir -p "$_BIN_DIR/local" "$_BIN_DIR/upstream" "$_DATA_DIR/smoke" \
    "$_OUTPUT_DIR" "$_SERVER_DIR"

_SERVER_LOCAL_PID=
_SERVER_UPSTREAM_PID=
_SERVER_LOCAL_PORT=
_SERVER_UPSTREAM_PORT=
_COMMAND=()
printf -v _PROBE_PADDING '%01024d' 0

_stop_pid() {
    local pid=$1
    local initial_signal=${2:-TERM}
    local attempt

    [[ -n "$pid" ]] || return 0
    if ! kill -0 "$pid" 2>/dev/null; then
        wait "$pid" 2>/dev/null || true
        return 0
    fi

    kill "-$initial_signal" "$pid" 2>/dev/null || true
    for ((attempt = 0; attempt < 100; attempt++)); do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" 2>/dev/null || true
            return 0
        fi
        sleep 0.05
    done

    kill -TERM "$pid" 2>/dev/null || true
    for ((attempt = 0; attempt < 40; attempt++)); do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" 2>/dev/null || true
            return 0
        fi
        sleep 0.05
    done

    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
}

_stop_servers() {
    _stop_pid "$_SERVER_LOCAL_PID"
    _stop_pid "$_SERVER_UPSTREAM_PID"
    _SERVER_LOCAL_PID=
    _SERVER_UPSTREAM_PID=
}

_cleanup() {
    _stop_servers
}
trap _cleanup EXIT

_root_for() {
    case "$1" in
    local) printf '%s\n' "$_LOCAL_ROOT" ;;
    upstream) printf '%s\n' "$_UPSTREAM_ROOT" ;;
    *) _die "unknown implementation: $1" ;;
    esac
}

_bin_for() {
    printf '%s/%s/%s\n' "$_BIN_DIR" "$1" "$2"
}

_tracked_tree_is_dirty() {
    local root=$1
    ! git -C "$root" diff --quiet --ignore-submodules -- ||
        ! git -C "$root" diff --cached --quiet --ignore-submodules --
}

_validate_revisions_for_run() {
    local root
    for root in "$_LOCAL_ROOT" "$_UPSTREAM_ROOT"; do
        if _tracked_tree_is_dirty "$root"; then
            _die "full benchmarks require no tracked changes in $root"
        fi
    done
}

_build_binaries() {
    local implementation root command_name
    printf 'Building both revisions with go=%s and -p=%s\n' \
        "$(command -v go)" "$_BUILD_PARALLELISM"
    for implementation in upstream local; do
        root="$(_root_for "$implementation")"
        for command_name in dserver dcat dgrep dmap dtail; do
            printf '  %-8s %s\n' "$implementation" "$command_name"
            (
                cd "$root"
                GOFLAGS= GOMAXPROCS="$_BUILD_PARALLELISM" go build \
                    -p="$_BUILD_PARALLELISM" -trimpath -buildvcs=false \
                    -o "$(_bin_for "$implementation" "$command_name")" \
                    "./cmd/$command_name"
            )
        done
    done
}

_generate_normal_data() {
    local destination=$1
    local target_bytes=$2
    local temporary="${destination}.tmp"
    LC_ALL=C awk -v target="$target_bytes" '
        BEGIN {
            bytes = 0
            for (i = 0; bytes < target; i++) {
                level = (i % 10 == 0 ? "ERROR" : "INFO")
                user = (i % 1000 == 0 ? "user999 " : sprintf("user%03d ", i % 997))
                line = sprintf("2026-01-01T00:00:%02dZ %s request=%09d %spath=/api/item/%04d status=%d payload=abcdefghijklmnopqrstuvwxyz0123456789", i % 60, level, i, user, i % 1000, (i % 10 == 0 ? 500 : 200))
                print line
                bytes += length(line) + 1
            }
        }
    ' > "$temporary"
    mv "$temporary" "$destination"
}

_generate_stats_data() {
    local destination=$1
    local target_bytes=$2
    local temporary="${destination}.tmp"
    LC_ALL=C awk -v target="$target_bytes" '
        BEGIN {
            bytes = 0
            for (i = 0; bytes < target; i++) {
                # Both numeric fields that have historically mapped to
                # $goroutines stay below the ten-group result cap.
                line = sprintf("INFO|0626-140021|1|stats.go:56|%d|%d|%d|0.%02d|1h0m0s|MAPREDUCE:STATS|hostname=host%d|currentConnections=%d|lifetimeConnections=%d", i % 8, i % 8, i % 10, i % 100, i % 8, i % 8, 1000 + i)
                print line
                bytes += length(line) + 1
            }
        }
    ' > "$temporary"
    mv "$temporary" "$destination"
}

_generate_follow_data() {
    local destination=$1
    local target_bytes=$2
    local temporary="${destination}.tmp"
    LC_ALL=C awk -v target="$target_bytes" '
        BEGIN {
            bytes = 0
            for (i = 0; bytes < target; i++) {
                line = sprintf("[2026-01-01 00:00:00.000] INFO - Follow benchmark line %09d payload abcdefghijklmnopqrstuvwxyz0123456789", i)
                print line
                bytes += length(line) + 1
            }
        }
    ' > "$temporary"
    mv "$temporary" "$destination"
}

_prepare_smoke_data() {
    _SMOKE_NORMAL="$_DATA_DIR/smoke/normal.log"
    _SMOKE_STATS="$_DATA_DIR/smoke/stats.log"
    _SMOKE_BURST="$_DATA_DIR/smoke/follow.log"
    _generate_normal_data "$_SMOKE_NORMAL" $((256 * 1024))
    _generate_stats_data "$_SMOKE_STATS" $((256 * 1024))
    _generate_follow_data "$_SMOKE_BURST" $((64 * 1024))
}

_prepare_full_data() {
    _FULL_NORMAL="$_DATA_DIR/normal_100mib.log"
    _FULL_LARGE="$_DATA_DIR/normal_1gib.log"
    _FULL_STATS="$_DATA_DIR/stats_100mib.log"
    _FULL_BURST="$_DATA_DIR/follow_10mib.log"
    printf 'Generating shared deterministic benchmark data in %s\n' "$_DATA_DIR"
    _generate_normal_data "$_FULL_NORMAL" $((100 * 1024 * 1024))
    _generate_normal_data "$_FULL_LARGE" $((1024 * 1024 * 1024))
    _generate_stats_data "$_FULL_STATS" $((100 * 1024 * 1024))
    _generate_follow_data "$_FULL_BURST" $((10 * 1024 * 1024))
}

_prepare_ssh_material() {
    local implementation cache_dir
    if [[ ! -f "$_AUTH_KEY" ]]; then
        ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$_AUTH_KEY"
    fi
    for implementation in local upstream; do
        cache_dir="$_SERVER_DIR/$implementation/cache"
        mkdir -p "$cache_dir"
        cp "$_AUTH_KEY.pub" "$cache_dir/$(id -un).authorized_keys"
        if [[ ! -f "$cache_dir/ssh_host_key" ]]; then
            ssh-keygen -q -t rsa -b 2048 -m PEM -N '' \
                -f "$cache_dir/ssh_host_key"
        fi
    done
}

_allocate_ports() {
    read -r _SERVER_LOCAL_PORT _SERVER_UPSTREAM_PORT < <(python3 - <<'PY'
import socket

sockets = []
try:
    for _ in range(2):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sockets.append(sock)
    print(*(sock.getsockname()[1] for sock in sockets))
finally:
    for sock in sockets:
        sock.close()
PY
    )
}

_wait_for_port() {
    local port=$1
    local pid=$2
    local attempt
    for ((attempt = 0; attempt < 200; attempt++)); do
        kill -0 "$pid" 2>/dev/null || return 1
        if python3 - "$port" <<'PY'
import socket
import sys

with socket.socket() as sock:
    sock.settimeout(0.1)
    sock.connect(("127.0.0.1", int(sys.argv[1])))
PY
        then
            return 0
        fi
        sleep 0.05
    done
    return 1
}

_start_servers() {
    local local_log="$_OUTPUT_DIR/local-dserver.log"
    local upstream_log="$_OUTPUT_DIR/upstream-dserver.log"
    _prepare_ssh_material
    _allocate_ports

    (
        cd "$_SERVER_DIR/local"
        exec env -u DTAIL_INTEGRATION_TEST_RUN_MODE \
            -u DTAIL_TURBOBOOST_DISABLE -u DTAIL_TURBOBOOST_ENABLE \
            "$(_bin_for local dserver)" --cfg none --logger stdout \
            --logLevel error --bindAddress 127.0.0.1 --port "$_SERVER_LOCAL_PORT"
    ) > "$local_log" 2>&1 &
    _SERVER_LOCAL_PID=$!

    (
        cd "$_SERVER_DIR/upstream"
        exec env -u DTAIL_INTEGRATION_TEST_RUN_MODE \
            -u DTAIL_TURBOBOOST_DISABLE -u DTAIL_TURBOBOOST_ENABLE \
            "$(_bin_for upstream dserver)" --cfg none --logger stdout \
            --logLevel error --bindAddress 127.0.0.1 --port "$_SERVER_UPSTREAM_PORT"
    ) > "$upstream_log" 2>&1 &
    _SERVER_UPSTREAM_PID=$!

    _wait_for_port "$_SERVER_LOCAL_PORT" "$_SERVER_LOCAL_PID" ||
        _die "local dserver did not start; see $local_log"
    _wait_for_port "$_SERVER_UPSTREAM_PORT" "$_SERVER_UPSTREAM_PID" ||
        _die "upstream dserver did not start; see $upstream_log"
}

_port_for() {
    case "$1" in
    local) printf '%s\n' "$_SERVER_LOCAL_PORT" ;;
    upstream) printf '%s\n' "$_SERVER_UPSTREAM_PORT" ;;
    *) _die "unknown implementation: $1" ;;
    esac
}

_server_pid_for() {
    case "$1" in
    local) printf '%s\n' "$_SERVER_LOCAL_PID" ;;
    upstream) printf '%s\n' "$_SERVER_UPSTREAM_PID" ;;
    *) _die "unknown implementation: $1" ;;
    esac
}

_set_command() {
    local implementation=$1
    local transport=$2
    local scenario=$3
    local normal_file=$4
    local large_file=$5
    local stats_file=$6
    local timeout_seconds=180
    local command_name
    local -a scenario_args
    local -a connection_args=()

    case "$scenario" in
    dcat_medium)
        command_name=dcat
        scenario_args=(--files "$normal_file")
        ;;
    dcat_large)
        command_name=dcat
        scenario_args=(--files "$large_file")
        timeout_seconds=600
        ;;
    dgrep_low)
        command_name=dgrep
        scenario_args=(--regex 'user999 ' --files "$normal_file")
        ;;
    dgrep_high)
        command_name=dgrep
        scenario_args=(--regex ERROR --files "$normal_file")
        ;;
    dmap_count)
        command_name=dmap
        scenario_args=(--query "$_COUNT_QUERY" --files "$stats_file")
        ;;
    dmap_aggregate)
        command_name=dmap
        scenario_args=(--query "$_AGGREGATE_QUERY" --files "$stats_file")
        ;;
    *) _die "unknown scenario: $scenario" ;;
    esac

    if [[ "$transport" == server ]]; then
        connection_args=(
            --trustAllHosts
            --servers "127.0.0.1:$(_port_for "$implementation")"
        )
    elif [[ "$transport" != serverless ]]; then
        _die "unknown transport: $transport"
    fi

    _COMMAND=(
        timeout --signal=TERM --kill-after=5 "${timeout_seconds}s"
        env -u DTAIL_AUTH_KEY_PATH -u DTAIL_INTEGRATION_TEST_RUN_MODE
        -u DTAIL_TURBOBOOST_DISABLE -u DTAIL_TURBOBOOST_ENABLE
        "DTAIL_SSH_PRIVATE_KEYFILE_PATH=$_AUTH_KEY"
        "$(_bin_for "$implementation" "$command_name")"
        --cfg none --plain --noColor --logger stdout --logLevel error
        "${connection_args[@]}" "${scenario_args[@]}"
    )
}

_canonicalize_dmap() {
    local source=$1
    local destination=$2
    {
        sed -n '1,3p' "$source"
        sed -n '4,$p' "$source" | sed '/^[[:space:]]*$/d' | LC_ALL=C sort
    } > "$destination"
}

_compare_files() {
    local expected=$1
    local actual=$2
    local description=$3
    if ! cmp -s "$expected" "$actual"; then
        diff -u "$expected" "$actual" | sed -n '1,120p' >&2 || true
        _die "$description outputs differ"
    fi
}

_capture_correctness_case() {
    local implementation=$1
    local transport=$2
    local scenario=$3
    local output_root=$4
    local output="$output_root/${transport}-${scenario}-${implementation}.out"
    local stderr="$output_root/${transport}-${scenario}-${implementation}.err"
    _set_command "$implementation" "$transport" "$scenario" \
        "$_SMOKE_NORMAL" "$_SMOKE_NORMAL" "$_SMOKE_STATS"
    if ! "${_COMMAND[@]}" > "$output" 2> "$stderr"; then
        _die "$implementation $transport $scenario failed; see $stderr"
    fi
}

_validate_dmap_total() {
    local output=$1
    local expected=$2
    local total
    total="$(
        sed -n '4,$p' "$output" |
            awk -F '|' '{gsub(/[[:space:]]/, "", $1); if ($1 ~ /^[0-9]+$/) total += $1} END {print total + 0}'
    )"
    [[ "$total" == "$expected" ]] ||
        _die "dmap count in $output was $total, expected $expected"
}

_smoke_transport() {
    local transport=$1
    local output_root="$_OUTPUT_DIR/correctness"
    local scenario implementation expected local_output upstream_output
    local expected_lines
    mkdir -p "$output_root"

    for scenario in dcat_medium dgrep_low dgrep_high dmap_count dmap_aggregate; do
        for implementation in upstream local; do
            _capture_correctness_case "$implementation" "$transport" "$scenario" "$output_root"
        done

        local_output="$output_root/${transport}-${scenario}-local.out"
        upstream_output="$output_root/${transport}-${scenario}-upstream.out"
        if [[ "$scenario" == dmap_* ]]; then
            _canonicalize_dmap "$local_output" "${local_output}.canonical"
            _canonicalize_dmap "$upstream_output" "${upstream_output}.canonical"
            _compare_files "${upstream_output}.canonical" "${local_output}.canonical" \
                "$transport $scenario"
            expected_lines="$(wc -l < "$_SMOKE_STATS")"
            _validate_dmap_total "$local_output" "$expected_lines"
            _validate_dmap_total "$upstream_output" "$expected_lines"
            continue
        fi

        case "$scenario" in
        dcat_medium) expected="$_SMOKE_NORMAL" ;;
        dgrep_low)
            expected="$output_root/expected-dgrep-low.out"
            grep 'user999 ' "$_SMOKE_NORMAL" > "$expected"
            ;;
        dgrep_high)
            expected="$output_root/expected-dgrep-high.out"
            grep ERROR "$_SMOKE_NORMAL" > "$expected"
            ;;
        esac
        _compare_files "$expected" "$upstream_output" "$transport $scenario upstream"
        _compare_files "$expected" "$local_output" "$transport $scenario local"
    done
}

_proc_cpu_ticks() {
    local pid=$1
    if [[ -r "/proc/$pid/stat" ]]; then
        awk '{print $14 + $15}' "/proc/$pid/stat"
    fi
}

_proc_user_system_ticks() {
    local pid=$1
    if [[ -r "/proc/$pid/stat" ]]; then
        awk '{print $14, $15}' "/proc/$pid/stat"
    fi
}

_proc_rss_kib() {
    local pid=$1
    if [[ -r "/proc/$pid/status" ]]; then
        awk '$1 == "VmRSS:" {print $2; exit}' "/proc/$pid/status"
    fi
}

_ticks_delta_seconds() {
    local before=$1
    local after=$2
    if [[ -n "$before" && -n "$after" ]]; then
        awk -v before="$before" -v after="$after" -v hz="$_CLOCK_TICKS" \
            'BEGIN {printf "%.9f", (after - before) / hz}'
    fi
}

_write_result() {
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' "$@" >> "$_RESULTS_CSV"
}

_run_follow_once() {
    local implementation=$1
    local iteration=$2
    local record=$3
    local burst_file=$4
    local server_pid client_pid
    local follow_file="$_OUTPUT_DIR/follow-work/follow-${implementation}-${iteration}-${record}.log"
    local output_file="$_OUTPUT_DIR/follow-${implementation}-${iteration}-${record}.out"
    local error_file="$_OUTPUT_DIR/follow-${implementation}-${iteration}-${record}.err"
    local probe marker live=0 delivered expected_lines status=ok
    local before_server_ticks after_server_ticks server_cpu server_rss
    local before_client_user before_client_system after_client_user after_client_system
    local client_user client_system started finished elapsed input_bytes attempt

    # Drop caches before every observation to ensure cold-start performance
    _drop_caches

    mkdir -p "$(dirname "$follow_file")"
    : > "$follow_file"
    : > "$output_file"
    server_pid="$(_server_pid_for "$implementation")"
    env -u DTAIL_AUTH_KEY_PATH -u DTAIL_INTEGRATION_TEST_RUN_MODE \
        -u DTAIL_TURBOBOOST_DISABLE -u DTAIL_TURBOBOOST_ENABLE \
        "DTAIL_SSH_PRIVATE_KEYFILE_PATH=$_AUTH_KEY" \
        "$(_bin_for "$implementation" dtail)" --cfg none --plain --noColor \
        --logger stdout --logLevel error --trustAllHosts \
        --servers "127.0.0.1:$(_port_for "$implementation")" \
        --shutdownAfter 120 --max 2147483647 --files "$follow_file" \
        > "$output_file" 2> "$error_file" &
    client_pid=$!

    probe="PROBE-${implementation}-${iteration}-${record}"
    for ((attempt = 0; attempt < 400; attempt++)); do
        # The local stdout logger batches up to 64 KiB. Padding makes the
        # readiness probe observable even if its idle flusher is delayed.
        printf '%s %s\n' "$probe" "$_PROBE_PADDING" >> "$follow_file"
        if grep -qF "$probe" "$output_file" 2>/dev/null; then
            live=1
            break
        fi
        if ! kill -0 "$client_pid" 2>/dev/null; then
            break
        fi
        sleep 0.05
    done

    if [[ "$live" != 1 ]]; then
        status=follow-not-live
        _stop_pid "$client_pid" TERM
        if [[ "$record" == yes ]]; then
            input_bytes="$(stat -c %s "$burst_file")"
            _write_result "sv_dtail_follow" "$implementation" "$iteration" '' '' '' '' '' '' \
                "$input_bytes" 0 "$status"
        fi
        return 1
    fi

    read -r before_client_user before_client_system < <(_proc_user_system_ticks "$client_pid")
    before_server_ticks="$(_proc_cpu_ticks "$server_pid")"
    marker="BENCHEND-${implementation}-${iteration}-${record}"
    started="$(date +%s%N)"
    cat "$burst_file" >> "$follow_file"
    printf '%s\n' "$marker" >> "$follow_file"
    for ((attempt = 0; attempt < 3000; attempt++)); do
        if grep -qF "$marker" "$output_file" 2>/dev/null; then
            break
        fi
        if ! kill -0 "$client_pid" 2>/dev/null; then
            status=client-exited
            break
        fi
        sleep 0.02
    done
    finished="$(date +%s%N)"
    if ! grep -qF "$marker" "$output_file" 2>/dev/null; then
        status=marker-timeout
    fi

    delivered="$(grep -c 'Follow benchmark line ' "$output_file" 2>/dev/null || true)"
    expected_lines="$(wc -l < "$burst_file")"
    if [[ "$delivered" != "$expected_lines" && "$status" == ok ]]; then
        status="delivered-${delivered}-of-${expected_lines}"
    fi

    read -r after_client_user after_client_system < <(_proc_user_system_ticks "$client_pid")
    after_server_ticks="$(_proc_cpu_ticks "$server_pid")"
    server_cpu="$(_ticks_delta_seconds "$before_server_ticks" "$after_server_ticks")"
    server_rss="$(_proc_rss_kib "$server_pid")"
    client_user="$(_ticks_delta_seconds "$before_client_user" "$after_client_user")"
    client_system="$(_ticks_delta_seconds "$before_client_system" "$after_client_system")"
    elapsed="$(awk -v started="$started" -v finished="$finished" \
        'BEGIN {printf "%.9f", (finished - started) / 1000000000}')"
    input_bytes="$(stat -c %s "$burst_file")"
    _stop_pid "$client_pid" TERM

    if [[ "$record" == yes ]]; then
        _write_result "sv_dtail_follow" "$implementation" "$iteration" "$elapsed" \
            "$client_user" "$client_system" '' "$server_cpu" "$server_rss" \
            "$input_bytes" "$delivered" "$status"
    fi
    [[ "$status" == ok ]]
}

_run_smoke_checks() {
    printf 'Running untimed serverless correctness checks\n'
    _smoke_transport serverless
    printf 'Running untimed matched client/server correctness checks\n'
    _start_servers
    _smoke_transport server
    if ! _run_follow_once upstream 0 no "$_SMOKE_BURST"; then
        _die "upstream dtail follow smoke check failed"
    fi
    if ! _run_follow_once local 0 no "$_SMOKE_BURST"; then
        _die "local dtail follow smoke check failed"
    fi
    _stop_servers
    printf 'Smoke checks passed: outputs and delivered follow records match.\n'
}

_measure_one() {
    local implementation=$1
    local transport=$2
    local scenario=$3
    local iteration=$4
    local input_bytes=$5
    local metrics elapsed client_user client_system client_rss returncode
    local server_pid= before_server_ticks= after_server_ticks= server_cpu= server_rss=
    local error_file="$_OUTPUT_DIR/measured-${transport}-${scenario}-${implementation}.err"
    local status=ok scenario_prefix

    # Drop caches before every observation to ensure cold-start performance
    _drop_caches

    _set_command "$implementation" "$transport" "$scenario" \
        "$_FULL_NORMAL" "$_FULL_LARGE" "$_FULL_STATS"
    if [[ "$transport" == server ]]; then
        server_pid="$(_server_pid_for "$implementation")"
        before_server_ticks="$(_proc_cpu_ticks "$server_pid")"
    fi

    metrics="$(python3 "$_MEASURE_HELPER" --stdout /dev/null \
        --stderr "$error_file" -- "${_COMMAND[@]}")"
    IFS=$'\t' read -r elapsed client_user client_system client_rss returncode <<< "$metrics"

    if [[ "$transport" == server ]]; then
        after_server_ticks="$(_proc_cpu_ticks "$server_pid")"
        server_cpu="$(_ticks_delta_seconds "$before_server_ticks" "$after_server_ticks")"
        server_rss="$(_proc_rss_kib "$server_pid")"
    fi
    if [[ "$returncode" != 0 ]]; then
        status="failed-rc${returncode}"
    fi

    if [[ "$transport" == server ]]; then
        scenario_prefix=sv
    else
        scenario_prefix=sl
    fi
    _write_result "${scenario_prefix}_${scenario}" "$implementation" "$iteration" \
        "$elapsed" "$client_user" "$client_system" "$client_rss" \
        "$server_cpu" "$server_rss" "$input_bytes" '' "$status"
    [[ "$status" == ok ]] ||
        printf 'warning: %s %s iteration %s failed (rc=%s)\n' \
            "$implementation" "$scenario" "$iteration" "$returncode" >&2
}

_warm_up() {
    local implementation=$1
    local transport=$2
    local scenario=$3
    _set_command "$implementation" "$transport" "$scenario" \
        "$_FULL_NORMAL" "$_FULL_LARGE" "$_FULL_STATS"
    "${_COMMAND[@]}" > /dev/null 2>> "$_OUTPUT_DIR/warmup-${transport}-${implementation}.err" ||
        _die "$implementation $transport $scenario warmup failed"
}

_run_pair() {
    local transport=$1
    local scenario=$2
    local iterations=$3
    local input_file=$4
    local input_bytes iteration implementation
    local -a order
    input_bytes="$(stat -c %s "$input_file")"
    printf 'Benchmarking %-10s %-18s (%s observations each)\n' \
        "$transport" "$scenario" "$iterations"

    _warm_up upstream "$transport" "$scenario"
    _warm_up local "$transport" "$scenario"
    for ((iteration = 1; iteration <= iterations; iteration++)); do
        if ((iteration % 2 == 1)); then
            order=(upstream local)
        else
            order=(local upstream)
        fi
        for implementation in "${order[@]}"; do
            _measure_one "$implementation" "$transport" "$scenario" \
                "$iteration" "$input_bytes"
        done
    done
}

_run_follow_pair() {
    local iterations=$1
    local iteration implementation
    local -a order
    printf 'Benchmarking server     dtail_follow       (%s observations each)\n' "$iterations"
    _run_follow_once upstream 0 no "$_FULL_BURST" ||
        _die "upstream dtail follow warmup failed"
    _run_follow_once local 0 no "$_FULL_BURST" ||
        _die "local dtail follow warmup failed"
    for ((iteration = 1; iteration <= iterations; iteration++)); do
        if ((iteration % 2 == 1)); then
            order=(upstream local)
        else
            order=(local upstream)
        fi
        for implementation in "${order[@]}"; do
            if ! _run_follow_once "$implementation" "$iteration" yes "$_FULL_BURST"; then
                printf 'warning: %s dtail follow iteration %s failed\n' \
                    "$implementation" "$iteration" >&2
            fi
        done
    done
}

_record_metadata() {
    local root implementation governor_file data_file
    {
        printf 'created_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf 'mode=%s\n' "$_MODE"
        printf 'hostname=%s\n' "$(hostname)"
        printf 'kernel=%s\n' "$(uname -srvmo)"
        printf 'go_binary=%s\n' "$(command -v go)"
        printf 'go_version=%s\n' "$(go version)"
        printf 'go_env=%s\n' "$(go env GOOS GOARCH CGO_ENABLED GOTOOLCHAIN | tr '\n' ' ')"
        printf 'logical_cpus=%s\n' "$(getconf _NPROCESSORS_ONLN)"
        governor_file=/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
        if [[ -r "$governor_file" ]]; then
            printf 'cpu_governor=%s\n' "$(< "$governor_file")"
        fi
        for implementation in upstream local; do
            root="$(_root_for "$implementation")"
            printf '\n[%s]\n' "$implementation"
            printf 'root=%s\n' "$root"
            printf 'commit=%s\n' "$(git -C "$root" rev-parse HEAD)"
            printf 'remote=%s\n' "$(git -C "$root" remote get-url origin 2>/dev/null || true)"
            printf 'go_directive=%s\n' "$(awk '$1 == "go" {print $2; exit}' "$root/go.mod")"
            printf 'tracked_dirty=%s\n' "$(_tracked_tree_is_dirty "$root" && printf yes || printf no)"
            printf 'status:\n'
            git -C "$root" status --short
            printf 'binary_sha256:\n'
            sha256sum "$_BIN_DIR/$implementation"/*
        done
        printf '\n[data_sha256]\n'
        for data_file in \
            "$_DATA_DIR/smoke/normal.log" \
            "$_DATA_DIR/smoke/stats.log" \
            "$_DATA_DIR/smoke/follow.log" \
            "$_DATA_DIR/normal_100mib.log" \
            "$_DATA_DIR/normal_1gib.log" \
            "$_DATA_DIR/stats_100mib.log" \
            "$_DATA_DIR/follow_10mib.log"; do
            if [[ -f "$data_file" ]]; then
                sha256sum "$data_file"
            fi
        done
    } > "$_METADATA_FILE"
}

_run_full_benchmark() {
    local standard_iterations=7
    local large_iterations=3
    local dmap_iterations=5
    local follow_iterations=3
    if [[ -n "$_ITERATIONS" ]]; then
        standard_iterations=$_ITERATIONS
        large_iterations=$_ITERATIONS
        dmap_iterations=$_ITERATIONS
        follow_iterations=$_ITERATIONS
    fi

    printf '%s\n' 'scenario,implementation,iteration,elapsed_seconds,client_user_seconds,client_system_seconds,client_max_rss_kib,server_cpu_seconds,server_rss_kib,input_bytes,delivered_records,status' > "$_RESULTS_CSV"

    _run_pair serverless dcat_medium "$standard_iterations" "$_FULL_NORMAL"
    _run_pair serverless dcat_large "$large_iterations" "$_FULL_LARGE"
    _run_pair serverless dgrep_low "$standard_iterations" "$_FULL_NORMAL"
    _run_pair serverless dgrep_high "$standard_iterations" "$_FULL_NORMAL"
    _run_pair serverless dmap_aggregate "$dmap_iterations" "$_FULL_STATS"

    _start_servers
    _run_pair server dcat_medium "$standard_iterations" "$_FULL_NORMAL"
    _run_pair server dgrep_low "$standard_iterations" "$_FULL_NORMAL"
    _run_pair server dgrep_high "$standard_iterations" "$_FULL_NORMAL"
    _run_pair server dmap_count "$dmap_iterations" "$_FULL_STATS"
    _run_pair server dmap_aggregate "$dmap_iterations" "$_FULL_STATS"
    _run_follow_pair "$follow_iterations"
    _stop_servers

    python3 "$_SUMMARY_HELPER" --input "$_RESULTS_CSV" \
        --csv "$_WORKDIR/summary.csv" --markdown "$_WORKDIR/report.md"
}

printf 'Work directory: %s\n' "$_WORKDIR"
if [[ "$_MODE" == run ]]; then
    _validate_revisions_for_run
fi
_build_binaries
_prepare_smoke_data
_run_smoke_checks

if [[ "$_MODE" == run ]]; then
    _prepare_full_data
    _record_metadata
    _run_full_benchmark
    printf 'Raw results: %s\n' "$_RESULTS_CSV"
    printf 'Summary:     %s\n' "$_WORKDIR/report.md"
else
    _record_metadata
    printf 'No benchmark measurements were collected.\n'
fi
