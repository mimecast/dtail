# PGO Command Execution Details

This document shows the exact commands executed during Profile-Guided Optimization (PGO) generation for DTail tools.

## Overview

When running `make pgo-generate` or `dtail-tools pgo`, the following commands are executed to generate performance profiles for each tool.

## Commands Executed

### 1. Building Baseline Binaries

```bash
go build -o pgo-build/dtail-baseline ./cmd/dtail
go build -o pgo-build/dcat-baseline ./cmd/dcat
go build -o pgo-build/dgrep-baseline ./cmd/dgrep
go build -o pgo-build/dmap-baseline ./cmd/dmap
go build -o pgo-build/dserver-baseline ./cmd/dserver
```

### 2. Profile Generation Commands

#### DTail Profile Generation
```bash
# Background log writer process
bash -c "for i in {1..200}; do 
  level=$((i % 4))
  case $level in 
    0) lvl=INFO;; 
    1) lvl=WARN;; 
    2) lvl=ERROR;; 
    3) lvl=DEBUG;; 
  esac
  echo \"[2025-07-04 15:00:00] $lvl - Test log line number $i with some additional text to process\" >> growing.log
  sleep 0.015
done"

# DTail command
pgo-build/dtail-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dtail_TIMESTAMP \
  -regex "ERROR|WARN" \
  -shutdownAfter 3 \
  pgo-profiles/growing.log
```

#### DCat Profile Generation
```bash
pgo-build/dcat-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dcat_TIMESTAMP \
  pgo-profiles/test.log
```

#### DGrep Profile Generation
```bash
pgo-build/dgrep-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dgrep_TIMESTAMP \
  -regex "ERROR|WARN" \
  pgo-profiles/test.log
```

#### DMap Profile Generation
```bash
pgo-build/dmap-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dmap_TIMESTAMP \
  -files pgo-profiles/test.csv \
  -query "select status, count(*) group by status"
```

#### DServer Profile Generation
```bash
# Start dserver with pprof endpoint
pgo-build/dserver-baseline \
  -cfg none \
  -pprof localhost:16060 \
  -port 12222

# Client commands to generate server load (run concurrently):
pgo-build/dcat-baseline \
  -cfg none \
  -server localhost:12222 \
  pgo-profiles/test.log

pgo-build/dgrep-baseline \
  -cfg none \
  -server localhost:12222 \
  -regex "ERROR|WARN" \
  pgo-profiles/test.log

pgo-build/dgrep-baseline \
  -cfg none \
  -server localhost:12222 \
  -regex "INFO.*action" \
  pgo-profiles/test.log

pgo-build/dmap-baseline \
  -cfg none \
  -server localhost:12222 \
  -files pgo-profiles/test.csv \
  -query "select status, count(*) group by status"

pgo-build/dmap-baseline \
  -cfg none \
  -server localhost:12222 \
  -files pgo-profiles/test.csv \
  -query "select department, avg(salary) group by department"

# Capture CPU profile via HTTP
curl http://localhost:16060/debug/pprof/profile?seconds=5 > dserver.pprof
```

### 3. Profile Merging

When multiple iterations are run, profiles are merged:

```bash
# Merge multiple profile iterations
go tool pprof -proto \
  pgo-profiles/dcat.pprof.0.pprof \
  pgo-profiles/dcat.pprof.1.pprof \
  > pgo-profiles/dcat.pprof
```

### 4. Building with PGO

```bash
# Build optimized binaries using profiles
go build -pgo=pgo-profiles/dcat.pprof -o pgo-build/dcat ./cmd/dcat
go build -pgo=pgo-profiles/dgrep.pprof -o pgo-build/dgrep ./cmd/dgrep
go build -pgo=pgo-profiles/dmap.pprof -o pgo-build/dmap ./cmd/dmap
go build -pgo=pgo-profiles/dtail.pprof -o pgo-build/dtail ./cmd/dtail
go build -pgo=pgo-profiles/dserver.pprof -o pgo-build/dserver ./cmd/dserver
```

### 5. Performance Comparison Commands

Quick benchmarks are run to compare baseline vs optimized:

```bash
# Baseline benchmark
pgo-build/dcat-baseline -cfg none -plain /tmp/pgo_bench.log

# Optimized benchmark
pgo-build/dcat -cfg none -plain /tmp/pgo_bench.log

# Similar commands for dgrep and dmap
pgo-build/dgrep-baseline -cfg none -plain -regex ERROR /tmp/pgo_bench.log
pgo-build/dmap-baseline -cfg none -plain -files /tmp/pgo_bench.csv -query "select count(*)"
```

## Test Data Generation

The PGO framework generates realistic test data:

### Log File (test.log)
- Contains timestamps, log levels (INFO, WARN, ERROR, DEBUG)
- Includes user actions, durations, and status information
- Default size: 1,000,000 lines (configurable with -datasize)

### CSV File (test.csv)
- Contains employee data with departments, salaries, status
- Used for MapReduce queries
- Default size: 100,000 rows (1/10 of log file size)

### Growing Log File (growing.log)
- Used specifically for dtail testing
- Simulates real-time log generation
- Writes ~200 lines over 3 seconds with mixed log levels

## Customization Options

### Adjust Test Data Size
```bash
dtail-tools pgo -datasize 5000000  # 5 million lines
```

### Run More Iterations
```bash
dtail-tools pgo -iterations 5  # Run 5 iterations per command
```

### Profile Specific Commands Only
```bash
dtail-tools pgo dcat dgrep  # Only optimize dcat and dgrep
```

### Verbose Output
```bash
dtail-tools pgo -v  # Show all command execution details
```

### Profile Generation Only
```bash
dtail-tools pgo -profileonly  # Skip building optimized binaries
```

## Notes

1. **Empty Profiles**: Some commands (like dtail) may generate empty profiles if they are I/O-bound. This is handled gracefully.

2. **DServer Profiling**: Uses HTTP pprof endpoint instead of command-line profiling to capture server-side performance data.

3. **Concurrent Execution**: Multiple client commands are run concurrently against dserver to generate realistic load patterns.

4. **Profile Quality**: The effectiveness of PGO depends on how well the test workload represents real-world usage patterns.