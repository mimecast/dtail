#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now exercise the SAME code path and any measured
# difference is noise. Kept as a historical harness only.
#
# Server-mode focused benchmark for turbo vs non-turbo comparison

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m'

BENCHMARK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$BENCHMARK_DIR")"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_DIR="$BENCHMARK_DIR/turbo_server_benchmark_$TIMESTAMP"

mkdir -p "$RESULTS_DIR"

echo -e "${BLUE}=== DTail Turbo Mode Server Benchmark ===${NC}"
echo -e "${BLUE}Focus: Server mode performance where turbo mode is designed to excel${NC}"
echo ""

# Create test data files of various sizes
create_test_files() {
    echo -e "${YELLOW}Creating test files...${NC}"
    
    # Small file (1MB)
    echo "Creating 1MB test file..."
    for i in {1..10000}; do
        echo "2025-01-01 10:00:00 INFO [app] Processing request ID=$i status=OK latency=42ms" >> "$RESULTS_DIR/test_1mb.log"
    done
    
    # Medium file (10MB)
    echo "Creating 10MB test file..."
    for i in {1..100000}; do
        echo "2025-01-01 10:00:00 INFO [app] Processing request ID=$i status=OK latency=42ms user=user$((i%100))" >> "$RESULTS_DIR/test_10mb.log"
    done
    
    echo -e "${GREEN}✓ Test files created${NC}"
}

# Function to run server benchmark
run_server_benchmark() {
    local mode=$1
    local port=$2
    local test_file=$3
    local output_prefix=$4
    
    # Turbo boost is on by default; the non-turbo arm sets DTAIL_TURBOBOOST_DISABLE.
    if [ "$mode" == "turbo" ]; then
        unset DTAIL_TURBOBOOST_DISABLE
    else
        export DTAIL_TURBOBOOST_DISABLE=yes
    fi
    
    # Start dserver
    ./dserver -port $port > "$RESULTS_DIR/${output_prefix}_server.log" 2>&1 &
    local server_pid=$!
    sleep 1
    
    # Run benchmark
    echo -e "  Running $mode mode with $(basename $test_file)..."
    
    # Run multiple iterations and capture timing
    local total_time=0
    local iterations=5
    
    for i in $(seq 1 $iterations); do
        local start_time=$(date +%s.%N)
        ./dcat --server localhost:$port "$test_file" > "$RESULTS_DIR/${output_prefix}_output_$i.txt" 2>&1
        local end_time=$(date +%s.%N)
        local elapsed=$(echo "$end_time - $start_time" | bc)
        total_time=$(echo "$total_time + $elapsed" | bc)
    done
    
    local avg_time=$(echo "scale=3; $total_time / $iterations" | bc)
    echo "$avg_time" > "$RESULTS_DIR/${output_prefix}_avg_time.txt"
    
    # Kill server
    kill $server_pid 2>/dev/null || true
    wait $server_pid 2>/dev/null || true
}

# Create test files
create_test_files

# Run benchmarks
echo ""
echo -e "${BLUE}Running Server Mode Benchmarks${NC}"
echo -e "${YELLOW}Note: Each test runs 5 iterations and reports average time${NC}"
echo ""

# Test with 1MB file
echo -e "${YELLOW}Testing with 1MB file:${NC}"
run_server_benchmark "non-turbo" 13001 "$RESULTS_DIR/test_1mb.log" "1mb_non_turbo"
run_server_benchmark "turbo" 13002 "$RESULTS_DIR/test_1mb.log" "1mb_turbo"

# Test with 10MB file
echo -e "${YELLOW}Testing with 10MB file:${NC}"
run_server_benchmark "non-turbo" 13003 "$RESULTS_DIR/test_10mb.log" "10mb_non_turbo"
run_server_benchmark "turbo" 13004 "$RESULTS_DIR/test_10mb.log" "10mb_turbo"

# Generate report
REPORT="$RESULTS_DIR/benchmark_report.md"
echo "# Turbo Mode Server Benchmark Report" > "$REPORT"
echo "" >> "$REPORT"
echo "**Date:** $(date)" >> "$REPORT"
echo "**Focus:** Server mode performance comparison" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Executive Summary" >> "$REPORT"
echo "" >> "$REPORT"
echo "Turbo mode is designed to optimize network transmission in server mode by:" >> "$REPORT"
echo "- Bypassing channel-based processing" >> "$REPORT"
echo "- Using direct network writes" >> "$REPORT"
echo "- Reducing memory allocations" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Results" >> "$REPORT"
echo "" >> "$REPORT"

# Read and format results
echo "### 1MB File Results" >> "$REPORT"
non_turbo_1mb=$(cat "$RESULTS_DIR/1mb_non_turbo_avg_time.txt")
turbo_1mb=$(cat "$RESULTS_DIR/1mb_turbo_avg_time.txt")
improvement_1mb=$(echo "scale=2; (($non_turbo_1mb - $turbo_1mb) / $non_turbo_1mb) * 100" | bc)
echo "- Non-Turbo: ${non_turbo_1mb}s (average of 5 runs)" >> "$REPORT"
echo "- Turbo: ${turbo_1mb}s (average of 5 runs)" >> "$REPORT"
echo "- **Improvement: ${improvement_1mb}%**" >> "$REPORT"
echo "" >> "$REPORT"

echo "### 10MB File Results" >> "$REPORT"
non_turbo_10mb=$(cat "$RESULTS_DIR/10mb_non_turbo_avg_time.txt")
turbo_10mb=$(cat "$RESULTS_DIR/10mb_turbo_avg_time.txt")
improvement_10mb=$(echo "scale=2; (($non_turbo_10mb - $turbo_10mb) / $non_turbo_10mb) * 100" | bc)
echo "- Non-Turbo: ${non_turbo_10mb}s (average of 5 runs)" >> "$REPORT"
echo "- Turbo: ${turbo_10mb}s (average of 5 runs)" >> "$REPORT"
echo "- **Improvement: ${improvement_10mb}%**" >> "$REPORT"
echo "" >> "$REPORT"

# Calculate file sizes
size_1mb=$(du -h "$RESULTS_DIR/test_1mb.log" | cut -f1)
size_10mb=$(du -h "$RESULTS_DIR/test_10mb.log" | cut -f1)
lines_1mb=$(wc -l < "$RESULTS_DIR/test_1mb.log")
lines_10mb=$(wc -l < "$RESULTS_DIR/test_10mb.log")

echo "### Throughput Analysis" >> "$REPORT"
echo "" >> "$REPORT"
echo "#### 1MB File ($size_1mb, $lines_1mb lines)" >> "$REPORT"
throughput_non_turbo_1mb=$(echo "scale=2; $lines_1mb / $non_turbo_1mb" | bc)
throughput_turbo_1mb=$(echo "scale=2; $lines_1mb / $turbo_1mb" | bc)
echo "- Non-Turbo: $throughput_non_turbo_1mb lines/sec" >> "$REPORT"
echo "- Turbo: $throughput_turbo_1mb lines/sec" >> "$REPORT"
echo "" >> "$REPORT"

echo "#### 10MB File ($size_10mb, $lines_10mb lines)" >> "$REPORT"
throughput_non_turbo_10mb=$(echo "scale=2; $lines_10mb / $non_turbo_10mb" | bc)
throughput_turbo_10mb=$(echo "scale=2; $lines_10mb / $turbo_10mb" | bc)
echo "- Non-Turbo: $throughput_non_turbo_10mb lines/sec" >> "$REPORT"
echo "- Turbo: $throughput_turbo_10mb lines/sec" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Key Findings" >> "$REPORT"
echo "" >> "$REPORT"
echo "1. **Server Mode Performance:**" >> "$REPORT"
if (( $(echo "$improvement_1mb > 0" | bc -l) )); then
    echo "   - Turbo mode shows ${improvement_1mb}% improvement for 1MB files" >> "$REPORT"
else
    echo "   - Turbo mode shows ${improvement_1mb}% degradation for 1MB files" >> "$REPORT"
fi
if (( $(echo "$improvement_10mb > 0" | bc -l) )); then
    echo "   - Turbo mode shows ${improvement_10mb}% improvement for 10MB files" >> "$REPORT"
else
    echo "   - Turbo mode shows ${improvement_10mb}% degradation for 10MB files" >> "$REPORT"
fi
echo "" >> "$REPORT"

echo "2. **Serverless Mode Issue:**" >> "$REPORT"
echo "   - Previous tests showed turbo mode is slower in serverless mode" >> "$REPORT"
echo "   - This is due to immediate per-line writes and unnecessary protocol formatting" >> "$REPORT"
echo "" >> "$REPORT"

echo "3. **Recommendations:**" >> "$REPORT"
echo "   - Use turbo mode only in server mode with network transmission" >> "$REPORT"
echo "   - Avoid turbo mode for serverless/direct output" >> "$REPORT"
echo "   - Turbo mode benefits increase with larger files and network latency" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Technical Details" >> "$REPORT"
echo "" >> "$REPORT"
echo "Turbo mode optimizations include:" >> "$REPORT"
echo "- Direct network writes bypassing channel pipeline" >> "$REPORT"
echo "- Pre-formatted line data to avoid repeated formatting" >> "$REPORT"
echo "- Memory pooling to reduce allocations" >> "$REPORT"
echo "- Buffered I/O with 256KB buffers" >> "$REPORT"
echo "" >> "$REPORT"

echo "Limitations:" >> "$REPORT"
echo "- Disabled for MapReduce operations due to concurrency issues" >> "$REPORT"
echo "- Less effective or counterproductive in serverless mode" >> "$REPORT"
echo "- May not show benefits for very small files" >> "$REPORT"

# Display summary
echo ""
echo -e "${BLUE}=== Benchmark Complete ===${NC}"
echo -e "${GREEN}Results saved to: $RESULTS_DIR${NC}"
echo -e "${GREEN}Report: $REPORT${NC}"
echo ""

# Show summary
echo -e "${BLUE}Performance Summary:${NC}"
echo -e "1MB File  - Turbo improvement: ${improvement_1mb}%"
echo -e "10MB File - Turbo improvement: ${improvement_10mb}%"

# Show the full report
echo ""
echo -e "${BLUE}Full Report:${NC}"
cat "$REPORT"