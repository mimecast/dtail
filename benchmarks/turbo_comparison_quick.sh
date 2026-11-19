#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now exercise the SAME code path and any measured
# difference is noise. Kept as a historical harness only.
#
# Quick benchmark comparison script for turbo mode vs non-turbo mode

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
RESULTS_DIR="$BENCHMARK_DIR/turbo_comparison_quick_$TIMESTAMP"

# Ensure results directory exists
mkdir -p "$RESULTS_DIR"

echo -e "${BLUE}=== DTail Turbo Mode Quick Benchmark Comparison ===${NC}"
echo -e "${BLUE}Timestamp: $TIMESTAMP${NC}"
echo -e "${BLUE}Results directory: $RESULTS_DIR${NC}"
echo ""

# Function to run benchmarks with specific configuration
run_benchmark() {
    local mode=$1
    local output_file=$2
    # Only run DCatSimple and DGrepSimplePattern benchmarks with shorter time
    local bench_pattern="Benchmark(DCatSimple|DGrepSimplePattern)$"
    
    echo -e "${YELLOW}Running quick benchmarks in $mode mode...${NC}"
    
    # Turbo boost is on by default; the non-turbo arm sets DTAIL_TURBOBOOST_DISABLE.
    if [ "$mode" == "turbo" ]; then
        unset DTAIL_TURBOBOOST_DISABLE
    else
        export DTAIL_TURBOBOOST_DISABLE=yes
    fi
    
    # Run benchmarks with shorter time and only small files
    cd "$PROJECT_ROOT"
    DTAIL_BENCH_SIZES=small go test -bench="$bench_pattern" -benchmem -benchtime=3s ./benchmarks > "$output_file" 2>&1
    
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
    
    echo "# Turbo Mode vs Non-Turbo Mode Quick Benchmark Comparison" > "$report_file"
    echo "" >> "$report_file"
    echo "**Date:** $(date)" >> "$report_file"
    echo "**System:** $(uname -a)" >> "$report_file"
    echo "**Go Version:** $(go version)" >> "$report_file"
    echo "" >> "$report_file"
    
    echo "## Summary" >> "$report_file"
    echo "" >> "$report_file"
    echo "This is a quick comparison of DTail performance with turbo mode enabled vs disabled." >> "$report_file"
    echo "Only small files (10MB) are tested for rapid results." >> "$report_file"
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
    
    # Simple performance calculation
    echo "## Performance Summary" >> "$report_file"
    echo "" >> "$report_file"
    
    # Extract and compare key metrics
    echo "### Quick Analysis" >> "$report_file"
    echo "" >> "$report_file"
    
    # DCat results
    echo "#### DCat Performance:" >> "$report_file"
    non_turbo_dcat=$(grep "BenchmarkDCatSimple/10MB" "$RESULTS_DIR/benchmark_non_turbo.txt" | head -1 | awk '{print $3}' || echo "N/A")
    turbo_dcat=$(grep "BenchmarkDCatSimple/10MB" "$RESULTS_DIR/benchmark_turbo.txt" | head -1 | awk '{print $3}' || echo "N/A")
    echo "- Non-Turbo: $non_turbo_dcat ns/op" >> "$report_file"
    echo "- Turbo: $turbo_dcat ns/op" >> "$report_file"
    
    # DGrep results
    echo "" >> "$report_file"
    echo "#### DGrep Performance:" >> "$report_file"
    non_turbo_dgrep=$(grep "BenchmarkDGrepSimplePattern/10MB" "$RESULTS_DIR/benchmark_non_turbo.txt" | head -1 | awk '{print $3}' || echo "N/A")
    turbo_dgrep=$(grep "BenchmarkDGrepSimplePattern/10MB" "$RESULTS_DIR/benchmark_turbo.txt" | head -1 | awk '{print $3}' || echo "N/A")
    echo "- Non-Turbo: $non_turbo_dgrep ns/op" >> "$report_file"
    echo "- Turbo: $turbo_dgrep ns/op" >> "$report_file"
    
    echo "" >> "$report_file"
    echo "## Notes" >> "$report_file"
    echo "" >> "$report_file"
    echo "- This is a quick benchmark using only small (10MB) files" >> "$report_file"
    echo "- For comprehensive results, run the full benchmark suite" >> "$report_file"
    echo "- Turbo mode improvements are typically more pronounced with larger files" >> "$report_file"
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
echo -e "${BLUE}=== Quick Benchmark Comparison Complete ===${NC}"
echo -e "${GREEN}Results saved to: $RESULTS_DIR${NC}"
echo -e "${GREEN}Report: $RESULTS_DIR/comparison_report.md${NC}"
echo ""

# Show the report
echo -e "${BLUE}Comparison Report:${NC}"
cat "$RESULTS_DIR/comparison_report.md"