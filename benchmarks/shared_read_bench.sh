#!/usr/bin/env bash
# Shared read benchmark: dserver cost of N sessions reading one file, with
# shared reads on (default) and off ("SharedReadsDisable": true).
#
# Scenarios:
#   follow     N dtail clients follow one plain file; 100 MiB (see -b) are
#              appended at once (burst). A client matches about 0.1% of the
#              lines.
#   follow-paced  the same, but the data are appended in 1 MiB writes
#              every 100 ms (about 10 MiB/s), like a busy log.
#   scheduled  N scheduled MapReduce jobs on one 100 MiB file (plain or
#              .gz); they start as one group, which dserver reads once with
#              sharing on and one job at a time with private reads off.
#
# Per run it records dserver CPU (utime+stime from /proc), elapsed time and,
# in separate runs with dserver under strace, the number of read syscalls on
# the input file and in total (the total includes socket reads; strace adds
# overhead, so CPU and time come from runs without it). Every
# run also checks that the output equals the expected one, so the numbers
# only count if sharing did not change what the sessions got.
#
# Usage:
#   benchmarks/shared_read_bench.sh [options] SCENARIO
#   SCENARIO: follow, follow-paced, scheduled or scheduled-gz
# Options:
#   -n N      sessions or jobs (default 4)
#   -r R      runs per mode, interleaving on and off (default 3)
#   -s        count read syscalls with strace instead of measuring CPU/time
#   -q SECS   wait up to SECS for a quiet machine before each run: 1-minute
#             load average below 1.0 and no other dserver, go test or make
#             process (default 0: do not wait; the load is recorded anyway)
#   -w DIR    work directory (default ${TMPDIR:-/tmp}/dtail-shared-read-bench)
#   -o FILE   append result rows to FILE as CSV (default: stdout only)
#   -p PORT   dserver port (default: the first free port from 24900 on)
#   -b BYTES  size of the generated input files (default 104857600, 100 MiB)
#   -d DIR    directory of prebuilt dserver/dtail binaries (default repo root)
#   -i FIRST  first run number (default 1); retains alternating mode order when
#             interleaving builds with separate -r 1 invocations
#
# Results are CSV rows:
#   scenario,input,mode,run,sessions,elapsed_s,cpu_s,file_reads,all_reads,
#   evictions,shared_log,load1,output_ok
set -euo pipefail

declare -r REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
declare BINARY_DIR="$REPO_DIR"
declare -i FIRST_RUN=1
declare -i DATA_BYTES=$((100 * 1024 * 1024))
declare -r CLK_TCK="$(getconf CLK_TCK)"
declare PORT=""

declare -i SESSIONS=4
declare -i RUNS=3
declare -i QUIET_WAIT=0
declare STRACE=no
declare WORK_DIR="${TMPDIR:-/tmp}/dtail-shared-read-bench"
declare RESULTS=""
declare SERVER_PID=""
declare -a CLIENT_PIDS=()

_die() {
    printf 'shared_read_bench: %s\n' "$*" >&2
    exit 1
}

_log() {
    printf '# %s\n' "$*" >&2
}

_cleanup() {
    local pid
    for pid in "${CLIENT_PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    if [[ -n "$SERVER_PID" ]]; then
        # Under strace SERVER_PID is strace; killing it detaches from
        # dserver, which would keep running and holding the port.
        if [[ "$STRACE" == yes ]]; then
            pid="$(_server_pid)" || pid=""
            [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true
        fi
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    CLIENT_PIDS=()
    SERVER_PID=""
}
trap _cleanup EXIT

_now() {
    date +%s.%N
}

# _poll SECS CMD...: runs CMD every 20 ms until it succeeds, fails after SECS.
_poll() {
    local -r timeout=$1
    shift
    local -r deadline=$(($(date +%s) + timeout))
    until "$@"; do
        (($(date +%s) < deadline)) || return 1
        sleep 0.02
    done
}

# The input files are named after their size, so that work directories can
# be shared between runs with different -b.
_normal_data() {
    printf '%s/data/normal_%d.log' "$WORK_DIR" "$DATA_BYTES"
}

_stats_data() {
    printf '%s/data/stats_%d.log' "$WORK_DIR" "$DATA_BYTES"
}

_prepare_data() {
    mkdir -p "$WORK_DIR/data" "$WORK_DIR/server/cache"
    local -r normal="$(_normal_data)"
    local -r stats="$(_stats_data)"
    if [[ ! -f "$normal" ]]; then
        _log "generating $normal"
        LC_ALL=C awk -v target="$DATA_BYTES" 'BEGIN {
            for (i = 0; bytes < target; i++) {
                level = (i % 10 == 0 ? "ERROR" : "INFO")
                user = (i % 1000 == 0 ? "user999 " : sprintf("user%03d ", i % 997))
                line = sprintf("2026-01-01T00:00:%02dZ %s request=%09d %spath=/api/item/%04d status=%d payload=abcdefghijklmnopqrstuvwxyz0123456789", i % 60, level, i, user, i % 1000, (i % 10 == 0 ? 500 : 200))
                print line
                bytes += length(line) + 1
            }
        }' > "$normal.tmp"
        mv "$normal.tmp" "$normal"
    fi
    if [[ ! -f "$stats" ]]; then
        _log "generating $stats"
        LC_ALL=C awk -v target="$DATA_BYTES" 'BEGIN {
            for (i = 0; bytes < target; i++) {
                line = sprintf("INFO|0626-140021|1|stats.go:56|%d|%d|%d|0.%02d|1h0m0s|MAPREDUCE:STATS|hostname=host%d|currentConnections=%d|lifetimeConnections=%d", i % 8, i % 8, i % 10, i % 100, i % 8, i % 8, 1000 + i)
                print line
                bytes += length(line) + 1
            }
        }' > "$stats.tmp"
        mv "$stats.tmp" "$stats"
    fi
    [[ -f "$stats.gz" ]] || gzip -k "$stats"
    if [[ ! -f "$WORK_DIR/id_rsa" ]]; then
        ssh-keygen -q -t rsa -b 2048 -m PEM -N '' -f "$WORK_DIR/id_rsa"
    fi
    cp "$WORK_DIR/id_rsa.pub" \
        "$WORK_DIR/server/cache/$(id -un).authorized_keys"
    if [[ ! -f "$WORK_DIR/server/cache/ssh_host_key" ]]; then
        ssh-keygen -q -t rsa -b 2048 -m PEM -N '' \
            -f "$WORK_DIR/server/cache/ssh_host_key"
    fi
    # The clients trust the server through their own known_hosts file, so
    # that they neither touch ~/.ssh/known_hosts nor race to update it.
    printf '[127.0.0.1]:%d %s\n' "$PORT" \
        "$(cut -d' ' -f1,2 "$WORK_DIR/server/cache/ssh_host_key.pub")" \
        > "$WORK_DIR/known_hosts"
    printf '{"Client": {"KnownHostsPath": "%s"}}\n' "$WORK_DIR/known_hosts" \
        > "$WORK_DIR/client.json"
}

_load1() {
    cut -d' ' -f1 /proc/loadavg
}

_machine_busy() {
    awk '{ exit !($1 >= 1.0) }' /proc/loadavg && return 0
    pgrep -x dserver > /dev/null && return 0
    pgrep -x make > /dev/null && return 0
    pgrep -f 'go test|\.test( |$)' > /dev/null && return 0
    return 1
}

_wait_quiet() {
    ((QUIET_WAIT > 0)) || return 0
    local -r deadline=$(($(date +%s) + QUIET_WAIT))
    while _machine_busy; do
        (($(date +%s) < deadline)) \
            || _die "machine not quiet within ${QUIET_WAIT}s (load $(_load1))"
        _log "machine busy (load $(_load1)), waiting 60s"
        sleep 60
    done
}

_cpu_ticks() {
    awk '{ print $14 + $15 }' "/proc/$1/stat"
}

# _write_config MODE FILE [SCHEDULE_JSON]
_write_config() {
    local -r mode=$1 file=$2 schedule=${3:-[]}
    local disable=false
    [[ "$mode" == off ]] && disable=true
    printf '{"Server": {"SharedReadsDisable": %s, "MaxConnections": %d, "MaxConcurrentCats": %d, "MaxConcurrentTails": %d, "Schedule": %s}}\n' \
        "$disable" $((SESSIONS * 4 + 4)) $((SESSIONS + 1)) \
        $((SESSIONS + 1)) "$schedule" > "$file"
}

# _start_server CFG LOG STRACE_OUT: sets SERVER_PID (the dserver process).
_start_server() {
    local -r cfg=$1 log=$2 strace_out=$3
    local -a cmd=("$BINARY_DIR/dserver" --cfg "$cfg" --logger stdout
        --logLevel info --bindAddress 127.0.0.1 --port "$PORT")
    if [[ "$STRACE" == yes ]]; then
        cmd=(strace -f -y -s 0 -e trace=read,pread64,readv,preadv
            -o "$strace_out" "${cmd[@]}")
    fi
    (cd "$WORK_DIR/server" \
        && exec env -u DTAIL_INTEGRATION_TEST_RUN_MODE "${cmd[@]}") \
        > "$log" 2>&1 &
    SERVER_PID=$!
    # "Binding server" is logged before the bind, which can still fail; only
    # a listening socket of this dserver shows that clients reach it and not
    # another process on the port.
    _poll 30 _server_listening "$log" \
        || _die "dserver did not listen on port $PORT, see $log"
}

# _port_in_use PORT: succeeds when something listens on TCP port PORT.
_port_in_use() {
    [[ -n "$(ss -Hltn "sport = :$1")" ]]
}

# _free_port: prints the first port from 24900 on that nothing listens on.
_free_port() {
    local -i port
    for ((port = 24900; port < 25900; port++)); do
        if ! _port_in_use "$port"; then
            printf '%d\n' "$port"
            return
        fi
    done
    _die "no free port in 24900-25899"
}

# _server_listening LOG: succeeds once this run's dserver listens on PORT and
# makes _poll give up at once when it exited or logged a start failure.
_server_listening() {
    local -r log=$1
    if ! kill -0 "$SERVER_PID" 2>/dev/null \
        || grep -q 'Unable to run dserver' "$log"; then
        _die "dserver failed to start on port $PORT, see $log"
    fi
    local pid
    pid="$(_server_pid)" || return 1
    ss -Hltnp "sport = :$PORT" | grep -q "pid=$pid,"
}

# _server_pid: the dserver process, also when it runs under strace.
_server_pid() {
    if [[ "$STRACE" == yes ]]; then
        pgrep -P "$SERVER_PID" -x dserver
    else
        printf '%s\n' "$SERVER_PID"
    fi
}

_stop_server() {
    kill -INT "$(_server_pid)" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""
}

# _read_calls STRACE_OUT FILE: prints "reads of FILE,all reads".
_read_calls() {
    local -r strace_out=$1 file=$2
    if [[ "$STRACE" != yes ]]; then
        printf 'n/a,n/a'
        return
    fi
    awk -v file="<$file>" '
        / (read|pread64|readv|preadv)\(/ {
            all++
            if (index($0, file)) { reads++ }
        }
        END { printf "%d,%d", reads, all }' "$strace_out"
}

_all_outputs_contain() {
    local -r marker=$1 dir=$2
    local -i i
    for ((i = 0; i < SESSIONS; i++)); do
        [[ -f "$dir/client$i.out" ]] || return 1
        tail -c 4096 "$dir/client$i.out" | grep -q "$marker" || return 1
    done
}

_sync_clients() {
    local -r file=$1 dir=$2
    local -i n=0
    until _all_outputs_contain BENCHSYNC "$dir"; do
        n+=1
        ((n < 3000)) || _die "clients did not sync"
        printf 'BENCHSYNC %d\n' "$n" >> "$file"
        sleep 0.02
    done
}

# _append_data PACE DATA FILE: appends DATA to FILE at once (burst) or in
# 1 MiB writes every 100 ms (paced).
_append_data() {
    local -r pace=$1 data=$2 file=$3
    if [[ "$pace" == burst ]]; then
        cat "$data" >> "$file"
        return
    fi
    local -i i
    local -ri chunks=$((($(stat -c %s "$data") + 1048575) / 1048576))
    for ((i = 0; i < chunks; i++)); do
        dd if="$data" bs=1M skip="$i" count=1 status=none >> "$file"
        sleep 0.1
    done
}

# _run_follow MODE RUN PACE: prints the CSV row.
_run_follow() {
    local -r mode=$1 run=$2 pace=$3
    local -r dir="$WORK_DIR/runs/follow-$pace-$mode-$run"
    local -r data="$(_normal_data)"
    local -r file="$dir/follow.log"
    rm -rf "$dir"
    mkdir -p "$dir"
    : > "$file"
    _write_config "$mode" "$dir/dtail.json"
    _start_server "$dir/dtail.json" "$dir/dserver.log" "$dir/strace.txt"

    local -i i
    for ((i = 0; i < SESSIONS; i++)); do
        DTAIL_AUTH_KEY_PATH="$WORK_DIR/id_rsa" "$BINARY_DIR/dtail" \
            --cfg "$WORK_DIR/client.json" --logger stdout --logLevel error \
            --plain --noColor --no-auth-key --servers "127.0.0.1:$PORT" \
            --files "$file" --grep 'user999 |BENCH' < /dev/null \
            > "$dir/client$i.out" 2>&1 &
        CLIENT_PIDS+=($!)
    done
    _sync_clients "$file" "$dir"

    local -r pid=$(_server_pid)
    local -r load=$(_load1)
    local -r cpu0=$(_cpu_ticks "$pid") t0=$(_now)
    _append_data "$pace" "$data" "$file"
    printf 'BENCHEND\n' >> "$file"
    _poll 900 _all_outputs_contain BENCHEND "$dir" \
        || _die "clients did not get all lines"
    local -r t1=$(_now) cpu1=$(_cpu_ticks "$pid")

    for i in "${CLIENT_PIDS[@]}"; do
        kill "$i" 2>/dev/null || true
    done
    CLIENT_PIDS=()
    _stop_server

    local ok=yes
    local -r expected="${data%.log}.expected"
    [[ -f "$expected" ]] || { grep -E 'user999 |BENCH' "$data" || true
        printf 'BENCHEND\n'; } > "$expected"
    for ((i = 0; i < SESSIONS; i++)); do
        grep -v BENCHSYNC "$dir/client$i.out" | cmp -s - "$expected" \
            || ok=no
    done
    _row "follow-$pace" plain "$mode" "$run" "$t0" "$t1" "$cpu0" "$cpu1" "$dir" \
        "$load" "$ok" "$file"
}

# _schedule_json INPUT DIR: N jobs with four different queries.
_schedule_json() {
    local -r input=$1 dir=$2
    local -a queries=(
        'from STATS select count($line),avg($goroutines) group by $hostname'
        'from STATS select max(lifetimeConnections),min(currentConnections) group by $hostname'
        'from STATS select count($line),sum($goroutines) group by $hostname'
        'from STATS select last($time),max($goroutines) group by $hostname'
    )
    local -i i
    local sep=""
    printf '['
    for ((i = 0; i < SESSIONS; i++)); do
        printf '%s{"Name": "bench%d", "Enable": true, "AllowFrom": ["localhost", "127.0.0.1"], "TimeRange": [0, 24], "Files": "%s", "Query": "%s", "Outfile": "%s/job%d.csv"}' \
            "$sep" "$i" "$input" "${queries[i % 4]}" "$dir" "$i"
        sep=", "
    done
    printf ']'
}

# _run_scheduled MODE RUN INPUT_KIND: prints the CSV row.
_run_scheduled() {
    local -r mode=$1 run=$2 kind=$3
    local -r dir="$WORK_DIR/runs/scheduled-$kind-$mode-$run"
    local input
    input="$(_stats_data)"
    [[ "$kind" == gz ]] && input+=".gz"
    rm -rf "$dir"
    mkdir -p "$dir"
    _write_config "$mode" "$dir/dtail.json" "$(_schedule_json "$input" "$dir")"
    local -r load=$(_load1)
    _start_server "$dir/dtail.json" "$dir/dserver.log" "$dir/strace.txt"
    local -r pid=$(_server_pid)
    local -r cpu0=$(_cpu_ticks "$pid")
    _poll 60 grep -q '|Starting job' "$dir/dserver.log" \
        || _die "jobs did not start"
    local -r t0=$(_now)
    _poll 1800 _jobs_done "$dir/dserver.log" || _die "jobs did not finish"
    local -r t1=$(_now) cpu1=$(_cpu_ticks "$pid")
    _stop_server

    local ok=yes
    grep -q 'exited with status [^0]' "$dir/dserver.log" && ok=no
    # The first run of an input, job count and dserver build is the
    # reference of the later ones: every job of it must have exited with
    # status 0. Keying it by the dserver binary's checksum makes a rebuild
    # take a new reference instead of comparing against an old build's.
    local -r build=$(sha256sum "$BINARY_DIR/dserver" | cut -c1-12)
    local -r reference="$WORK_DIR/runs/scheduled-$kind-$DATA_BYTES-n$SESSIONS-$build-reference"
    if [[ ! -d "$reference" && "$ok" == yes ]]; then
        rm -rf "$reference.tmp"
        mkdir -p "$reference.tmp"
        cp "$dir"/job*.csv "$reference.tmp/"
        mv "$reference.tmp" "$reference"
    fi
    [[ -d "$reference" ]] || ok=no
    local f
    for f in "$reference"/job*.csv; do
        cmp -s "$f" "$dir/$(basename "$f")" || ok=no
    done
    _row scheduled "$kind" "$mode" "$run" "$t0" "$t1" "$cpu0" "$cpu1" \
        "$dir" "$load" "$ok" "$input"
}

_jobs_done() {
    (($(grep -c 'Job bench[0-9]* exited with status' "$1") >= SESSIONS))
}

_row() {
    local -r scenario=$1 input=$2 mode=$3 run=$4 t0=$5 t1=$6 cpu0=$7 cpu1=$8
    local -r dir=$9 load=${10} ok=${11} file=${12}
    local -r log="$dir/dserver.log"
    local elapsed cpu evictions shared
    elapsed=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.2f", b - a }')
    cpu=$(awk -v a="$cpu0" -v b="$cpu1" -v hz="$CLK_TCK" \
        'BEGIN { printf "%.2f", (b - a) / hz }')
    evictions=$(grep -c 'evicted a slow subscriber' "$log" || true)
    shared=$(grep -c -E 'Shared (follow|one-shot) read started' "$log" || true)
    if [[ "$STRACE" == yes ]]; then
        elapsed=n/a
        cpu=n/a
    fi
    local -r row="$scenario,$input,$mode,$run,$SESSIONS,$elapsed,$cpu,$(_read_calls "$dir/strace.txt" "$file"),$evictions,$shared,$load,$ok"
    printf '%s\n' "$row"
    [[ -z "$RESULTS" ]] || printf '%s\n' "$row" >> "$RESULTS"
}

_run_one() {
    local -r scenario=$1 mode=$2 run=$3
    _wait_quiet
    case "$scenario" in
        follow) _run_follow "$mode" "$run" burst ;;
        follow-paced) _run_follow "$mode" "$run" paced ;;
        scheduled) _run_scheduled "$mode" "$run" plain ;;
        scheduled-gz) _run_scheduled "$mode" "$run" gz ;;
        *) _die "unknown scenario $scenario" ;;
    esac
}

main() {
    local opt
    while getopts 'n:r:sq:w:o:p:b:d:i:' opt; do
        case "$opt" in
            n) SESSIONS=$OPTARG ;;
            r) RUNS=$OPTARG ;;
            s) STRACE=yes ;;
            q) QUIET_WAIT=$OPTARG ;;
            w) WORK_DIR=$OPTARG ;;
            o) RESULTS=$OPTARG ;;
            p) PORT=$OPTARG ;;
            b) DATA_BYTES=$OPTARG ;;
            d) BINARY_DIR=$OPTARG ;;
            i)
                [[ "$OPTARG" =~ ^[1-9][0-9]*$ ]] \
                    || _die "first run must be a positive integer"
                FIRST_RUN=$OPTARG
                ;;
            *) _die "unknown option" ;;
        esac
    done
    shift $((OPTIND - 1))
    (($# == 1)) || _die "usage: $0 [options] SCENARIO (see the header)"
    local -r scenario=$1
    ((DATA_BYTES > 0)) || _die "-b needs a positive number of bytes"
    ((FIRST_RUN > 0 && RUNS > 0)) || _die "run numbers must be positive"
    BINARY_DIR="$(cd "$BINARY_DIR" && pwd -P)"
    [[ -x "$BINARY_DIR/dserver" && -x "$BINARY_DIR/dtail" ]] \
        || _die "build first: make build"
    if [[ "$STRACE" == yes ]]; then
        command -v strace > /dev/null || _die "strace not installed"
    fi
    command -v ss > /dev/null || _die "ss (iproute) not installed"
    [[ -n "$PORT" ]] || PORT="$(_free_port)"
    ! _port_in_use "$PORT" || _die "port $PORT is in use, pick another with -p"
    _log "dserver port $PORT"
    _prepare_data

    local -i run
    for ((run = FIRST_RUN; run < FIRST_RUN + RUNS; run++)); do
        # Interleave the modes and alternate which one goes first.
        if ((run % 2)); then
            _run_one "$scenario" on "$run"
            _run_one "$scenario" off "$run"
        else
            _run_one "$scenario" off "$run"
            _run_one "$scenario" on "$run"
        fi
    done
}

main "$@"
