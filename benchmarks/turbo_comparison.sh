#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now exercise the SAME code path and any measured
# difference is noise. Kept as a historical harness only.
#
# Benchmark comparison script for turbo mode vs non-turbo mode

set -e

# Colors for output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Configuration
BENCHMARK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$BENCHMARK_DIR")"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_DIR="$BENCHMARK_DIR/turbo_comparison_$TIMESTAMP"

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

echo -e "${BLUE}=== DTail Turbo Mode Benchmark Comparison ===${NC}"
echo -e "${BLUE}Timestamp: $TIMESTAMP${NC}"
echo -e "${BLUE}Results directory: $RESULTS_DIR${NC}"
echo ""

# Function to run benchmarks with specific configuration
run_benchmark() {
    local mode=$1
    local output_file=$2
    local bench_pattern=${3:-"Benchmark(DCat|DGrep)"}  # Focus on DCat and DGrep (turbo mode doesn't affect DMap)
    
    echo -e "${YELLOW}Running benchmarks in $mode mode...${NC}"
    
    # Turbo boost is enabled by default and disabled via DTAIL_TURBOBOOST_DISABLE.
    # The turbo arm clears the flag; the non-turbo arm sets it to "yes".
    if [ "$mode" == "turbo" ]; then
        unset DTAIL_TURBOBOOST_DISABLE
    else
        export DTAIL_TURBOBOOST_DISABLE=yes
    fi
    
    # Run benchmarks with memory profiling
    cd "$PROJECT_ROOT"
    go test -bench="$bench_pattern" -benchmem -benchtime=10s ./benchmarks > "$output_file" 2>&1
    
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}✓ $mode benchmarks completed${NC}"
    else
        echo -e "${RED}✗ $mode benchmarks failed${NC}"
        cat "$output_file"
        exit 1
    fi
}

# Function to extract benchmark results
extract_results() {
    local file=$1
    grep -E "^Benchmark" "$file" | grep -v "FAIL" || true
}

# Function to generate comparison report
generate_report() {
    local report_file="$RESULTS_DIR/comparison_report.md"
    
    echo "# Turbo Mode vs Non-Turbo Mode Benchmark Comparison" > "$report_file"
    echo "" >> "$report_file"
    echo "**Date:** $(date)" >> "$report_file"
    echo "**System:** $(uname -a)" >> "$report_file"
    echo "**Go Version:** $(go version)" >> "$report_file"
    echo "" >> "$report_file"
    
    echo "## Summary" >> "$report_file"
    echo "" >> "$report_file"
    echo "This report compares DTail performance with turbo mode enabled vs disabled." >> "$report_file"
    echo "Turbo boost is enabled by default and disabled via the \`DTAIL_TURBOBOOST_DISABLE\` environment variable." >> "$report_file"
    echo "" >> "$report_file"
    
    echo "## Raw Results" >> "$report_file"
    echo "" >> "$report_file"
    echo "### Non-Turbo Mode" >> "$report_file"
    echo '```' >> "$report_file"
    extract_results "$RESULTS_DIR/benchmark_non_turbo.txt" >> "$report_file"
    echo '```' >> "$report_file"
    echo "" >> "$report_file"
    
    echo "### Turbo Mode" >> "$report_file"
    echo '```' >> "$report_file"
    extract_results "$RESULTS_DIR/benchmark_turbo.txt" >> "$report_file"
    echo '```' >> "$report_file"
    echo "" >> "$report_file"
    
    # Generate comparison using benchstat if available
    if command -v benchstat &> /dev/null; then
        echo "## Statistical Comparison (benchstat)" >> "$report_file"
        echo '```' >> "$report_file"
        benchstat "$RESULTS_DIR/benchmark_non_turbo.txt" "$RESULTS_DIR/benchmark_turbo.txt" >> "$report_file" 2>&1 || true
        echo '```' >> "$report_file"
        echo "" >> "$report_file"
    fi
    
    echo "## Performance Analysis" >> "$report_file"
    echo "" >> "$report_file"
    
    # Simple performance comparison
    echo "### Performance Improvements" >> "$report_file"
    echo "" >> "$report_file"
    
    # Create a simple Python script for analysis if Python is available
    if command -v python3 &> /dev/null; then
        python3 - <<EOF >> "$report_file"
import re

def parse_benchmark_line(line):
    parts = line.split()
    if len(parts) >= 5:
        name = parts[0]
        ns_op = float(parts[2])
        return name, ns_op
    return None, None

# Read results
with open("$RESULTS_DIR/benchmark_non_turbo.txt", "r") as f:
    non_turbo_lines = [l.strip() for l in f if l.startswith("Benchmark")]

with open("$RESULTS_DIR/benchmark_turbo.txt", "r") as f:
    turbo_lines = [l.strip() for l in f if l.startswith("Benchmark")]

# Parse and compare
non_turbo_results = {}
turbo_results = {}

for line in non_turbo_lines:
    name, ns_op = parse_benchmark_line(line)
    if name:
        non_turbo_results[name] = ns_op

for line in turbo_lines:
    name, ns_op = parse_benchmark_line(line)
    if name:
        turbo_results[name] = ns_op

# Calculate improvements
print("| Benchmark | Non-Turbo (ns/op) | Turbo (ns/op) | Improvement |")
print("|-----------|-------------------|---------------|-------------|")

for name in sorted(set(non_turbo_results.keys()) & set(turbo_results.keys())):
    non_turbo_ns = non_turbo_results[name]
    turbo_ns = turbo_results[name]
    improvement = ((non_turbo_ns - turbo_ns) / non_turbo_ns) * 100
    print(f"| {name} | {non_turbo_ns:.0f} | {turbo_ns:.0f} | {improvement:.1f}% |")
EOF
    fi
    
    echo "" >> "$report_file"
    echo "## Notes" >> "$report_file"
    echo "" >> "$report_file"
    echo "- Turbo mode bypasses channel-based processing for improved performance" >> "$report_file"
    echo "- Turbo mode is automatically disabled for MapReduce operations due to concurrency considerations" >> "$report_file"
    echo "- Performance improvements are most noticeable with large files and high-throughput operations" >> "$report_file"
    echo "" >> "$report_file"
}

# Build binaries first
echo -e "${YELLOW}Building DTail binaries...${NC}"
cd "$PROJECT_ROOT"
make build
if [ $? -ne 0 ]; then
    echo -e "${RED}Failed to build binaries${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Build completed${NC}"
echo ""

# Run benchmarks in both modes
run_benchmark "non-turbo" "$RESULTS_DIR/benchmark_non_turbo.txt"
echo ""
run_benchmark "turbo" "$RESULTS_DIR/benchmark_turbo.txt"
echo ""

# Generate comparison report
echo -e "${YELLOW}Generating comparison report...${NC}"
generate_report
echo -e "${GREEN}✓ Report generated${NC}"
echo ""

# Display summary
echo -e "${BLUE}=== Benchmark Comparison Complete ===${NC}"
echo -e "${GREEN}Results saved to: $RESULTS_DIR${NC}"
echo -e "${GREEN}Report: $RESULTS_DIR/comparison_report.md${NC}"
echo ""

# Show quick summary
echo -e "${BLUE}Quick Summary:${NC}"
if command -v benchstat &> /dev/null; then
    benchstat -alpha 0.05 "$RESULTS_DIR/benchmark_non_turbo.txt" "$RESULTS_DIR/benchmark_turbo.txt" | head -20
else
    echo "Install benchstat for detailed statistical comparison:"
    echo "  go install golang.org/x/perf/cmd/benchstat@latest"
fi