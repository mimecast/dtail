#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE (and the older
# DTAIL_TURBOBOOST_ENABLE) are now inert no-ops. The former "turbo" path is the
# single, default read/output path, so the "turbo" and "non-turbo" arms below now
# exercise the SAME code path and any measured difference is noise. Kept as a
# historical harness (it produced the dated benchmark reports under benchmarks/
# and docs/); it no longer performs a meaningful turbo-vs-non-turbo comparison.
#
# Turbo vs non-turbo end-to-end benchmark for the DTail tools
# (dcat, dgrep, dmap, dtail follow).
#
# Turbo is ON by default; non-turbo runs set DTAIL_TURBOBOOST_DISABLE=yes on
# the process that runs the server handlers: the client itself in serverless
# mode, dserver in server mode. Server mode uses real SSH auth (dedicated
# throwaway key + CacheDir authorized_keys) instead of
# DTAIL_INTEGRATION_TEST_RUN_MODE, because integration-test mode force-
# disables turbo (internal/config/initializer.go) and would compare
# non-turbo against non-turbo.
#
# Note: turbo boost is opt-out via DTAIL_TURBOBOOST_DISABLE (default-on since
# commit aa2f547); the stale DTAIL_TURBOBOOST_ENABLE env var is no longer read.
# This script does whole-tool A/B timing; the companion benchmark scripts
# (e.g. turbo_comparison.sh) drive the same DTAIL_TURBOBOOST_DISABLE toggle.
#
# Usage: ./turbo_vs_normal_bench.sh [workdir]
#   workdir defaults to a fresh mktemp dir; results land in
#   $workdir/results.csv. Requires binaries built in the repo root
#   (make build) and benchmarks/testdata/{medium,large}.log (make of the
#   regular benchmark suite generates them, or see testdata_generator.go).
set -u

BENCHMARK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$BENCHMARK_DIR")"
DATA="$BENCHMARK_DIR/testdata"
WORKDIR="${1:-$(mktemp -d /tmp/dtail_turbo_bench.XXXXXX)}"
CSV="$WORKDIR/results.csv"
PORT_TURBO=14201
PORT_NOTURBO=14202
mkdir -p "$WORKDIR/server/cache" "$WORKDIR/data"

echo "Workdir: $WORKDIR"
echo "scenario,mode,iter,seconds,note" > "$CSV"

# --- SSH auth for server mode ---
KEY=$WORKDIR/bench_key
if [ ! -f "$KEY" ]; then
    ssh-keygen -t rsa -b 2048 -N "" -f "$KEY" -q
    cp "$KEY.pub" "$WORKDIR/server/cache/$(id -un).authorized_keys"
fi
CLIENT_COMMON=(--cfg none --plain --noColor --trustAllHosts --auth-key-path "$KEY")

# --- test data ---
STATS=$WORKDIR/data/stats_100mb.log
if [ ! -f "$STATS" ]; then
    awk 'BEGIN{for(i=0;i<800000;i++){printf "INFO|0626-140021|1|stats.go:56|%d|%d|%d|0.%02d|1h0m0s|MAPREDUCE:STATS|hostname=host%d|currentConnections=%d|lifetimeConnections=%d\n", i%8, i%100, i%10, i%100, i%50, i%20, 1000+i}}' > "$STATS"
fi
CHUNK=$WORKDIR/data/burst_10mb.log
if [ ! -f "$CHUNK" ]; then
    awk 'BEGIN{for(i=0;i<100000;i++) printf "[2026-07-10 12:00:00.000] INFO - Follow benchmark line %d payload abcdefghijklmnopqrstuvwxyz0123456789\n", i}' > "$CHUNK"
fi

now_ns() { date +%s%N; }

time_run() { # scenario mode iter cmd...
    local scenario=$1 mode=$2 iter=$3; shift 3
    local t0 t1 rc
    t0=$(now_ns)
    "$@" > /dev/null 2>>"$WORKDIR/client_stderr.log"
    rc=$?
    t1=$(now_ns)
    if [ $rc -ne 0 ]; then
        echo "FAILED rc=$rc: $scenario $mode iter $iter" >&2
        echo "$scenario,$mode,$iter,,failed-rc$rc" >> "$CSV"
        return 1
    fi
    echo "$scenario,$mode,$iter,$(awk "BEGIN{printf \"%.3f\", ($t1-$t0)/1e9}")," >> "$CSV"
}

run_serverless() { # scenario N cmd... (A/B alternating iterations)
    local scenario=$1 n=$2; shift 2
    "$@" > /dev/null 2>/dev/null                                # warmup turbo
    DTAIL_TURBOBOOST_DISABLE=yes "$@" > /dev/null 2>/dev/null   # warmup non-turbo
    for i in $(seq 1 "$n"); do
        time_run "$scenario" turbo "$i" "$@"
        time_run "$scenario" noturbo "$i" env DTAIL_TURBOBOOST_DISABLE=yes "$@"
    done
    echo "done: $scenario"
}

run_server() { # scenario N timeout clientcmd... (__PORT__ replaced per mode)
    local scenario=$1 n=$2 tmo=$3; shift 3
    local args_turbo=() args_noturbo=() a
    for a in "$@"; do
        args_turbo+=("${a//__PORT__/$PORT_TURBO}")
        args_noturbo+=("${a//__PORT__/$PORT_NOTURBO}")
    done
    timeout "$tmo" "${args_turbo[@]}" > /dev/null 2>/dev/null    # warmup
    timeout "$tmo" "${args_noturbo[@]}" > /dev/null 2>/dev/null  # warmup
    for i in $(seq 1 "$n"); do
        time_run "$scenario" turbo "$i" timeout "$tmo" "${args_turbo[@]}"
        time_run "$scenario" noturbo "$i" timeout "$tmo" "${args_noturbo[@]}"
    done
    echo "done: $scenario"
}

start_servers() {
    cd "$WORKDIR/server" || exit 1
    "$ROOT/dserver" --cfg none --logger stdout --logLevel error \
        --bindAddress localhost --port $PORT_TURBO > dserver_turbo.log 2>&1 &
    SRV_TURBO_PID=$!
    DTAIL_TURBOBOOST_DISABLE=yes "$ROOT/dserver" --cfg none --logger stdout \
        --logLevel error --bindAddress localhost --port $PORT_NOTURBO \
        > dserver_noturbo.log 2>&1 &
    SRV_NOTURBO_PID=$!
    sleep 2
}

stop_servers() {
    kill $SRV_TURBO_PID $SRV_NOTURBO_PID 2>/dev/null
    wait $SRV_TURBO_PID $SRV_NOTURBO_PID 2>/dev/null
}

# dtail follow: append a 10MB burst to a followed file; measure time until the
# end marker reaches the client, and count delivered lines (the non-turbo tail
# path drops lines when the consumer lags, so completeness matters here).
run_dtail_follow() {
    local scenario=$1 n=$2 mode port i
    for mode in turbo noturbo; do
        if [ "$mode" = turbo ]; then port=$PORT_TURBO; else port=$PORT_NOTURBO; fi
        for i in $(seq 0 "$n"); do  # iter 0 = warmup
            local follow=$WORKDIR/data/follow_$mode.log
            local out=$WORKDIR/data/follow_out_${mode}_$i.txt
            : > "$follow"
            "$ROOT/dtail" "${CLIENT_COMMON[@]}" --servers "localhost:$port" \
                --files "$follow" > "$out" 2>/dev/null &
            local dtail_pid=$!
            local probes=0  # wait until the follow stream is live
            while true; do
                echo "PROBE-$mode-$i-$probes" >> "$follow"; sleep 0.2
                grep -q "PROBE-$mode-$i-" "$out" 2>/dev/null && break
                probes=$((probes+1)); [ $probes -gt 75 ] && break
            done
            local marker="BENCHEND-$mode-$i" t0 t1 w=0
            t0=$(now_ns)
            cat "$CHUNK" >> "$follow"; echo "$marker" >> "$follow"
            while ! grep -q "$marker" "$out" 2>/dev/null; do
                sleep 0.05; w=$((w+1)); [ $w -gt 1800 ] && break
            done
            t1=$(now_ns)
            sleep 1  # let trailing lines land before counting
            local lines
            lines=$(grep -c "Follow benchmark line" "$out")
            kill $dtail_pid 2>/dev/null; wait $dtail_pid 2>/dev/null
            local res=ok; [ $w -gt 1800 ] && res="timeout_gt90s"
            if [ "$i" -gt 0 ]; then
                echo "$scenario,$mode,$i,$(awk "BEGIN{printf \"%.3f\", ($t1-$t0)/1e9}"),$res delivered=$lines/100000" >> "$CSV"
            fi
            rm -f "$out"
        done
    done
    echo "done: $scenario"
}

echo "=== Serverless scenarios ==="
run_serverless sl_dcat_100mb 7 "$ROOT/dcat" --cfg none --plain "$DATA/medium.log"
run_serverless sl_dcat_1gb 3 "$ROOT/dcat" --cfg none --plain "$DATA/large.log"
run_serverless sl_dgrep_low_100mb 7 "$ROOT/dgrep" --cfg none --plain --regex "user999 " --files "$DATA/medium.log"
run_serverless sl_dgrep_high_100mb 7 "$ROOT/dgrep" --cfg none --plain --regex "ERROR" --files "$DATA/medium.log"
run_serverless sl_dmap_agg_100mb 5 "$ROOT/dmap" --cfg none --plain \
    --query "from STATS select count(\$line),avg(\$goroutines),max(\$goroutines),sum(\$goroutines) group by \$goroutines" \
    --files "$STATS"

echo "=== Server-mode scenarios ==="
start_servers
run_server sv_dcat_100mb 7 120 "$ROOT/dcat" "${CLIENT_COMMON[@]}" --servers localhost:__PORT__ --files "$DATA/medium.log"
run_server sv_dgrep_low_100mb 7 120 "$ROOT/dgrep" "${CLIENT_COMMON[@]}" --servers localhost:__PORT__ --regex "user999 " --files "$DATA/medium.log"
run_server sv_dgrep_high_100mb 7 120 "$ROOT/dgrep" "${CLIENT_COMMON[@]}" --servers localhost:__PORT__ --regex "ERROR" --files "$DATA/medium.log"
# NOTE: as of 2026-07-10 the turbo halves of the two dmap scenarios hang
# (client never exits; see turbo_vs_normal_report_20260710.md). The timeout
# converts the hang into failed-rc124 rows instead of blocking the suite.
run_server sv_dmap_count_100mb 5 120 "$ROOT/dmap" "${CLIENT_COMMON[@]}" --servers localhost:__PORT__ \
    --query "from STATS select count(\$line) group by \$hostname" --files "$STATS"
run_server sv_dmap_agg_100mb 5 120 "$ROOT/dmap" "${CLIENT_COMMON[@]}" --servers localhost:__PORT__ \
    --query "from STATS select count(\$line),avg(\$goroutines),max(\$goroutines),sum(\$goroutines) group by \$goroutines" \
    --files "$STATS"
run_dtail_follow sv_dtail_follow_10mb 3
stop_servers

echo "=== ALL DONE ==="
echo "Results: $CSV"
