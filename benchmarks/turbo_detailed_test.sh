#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now exercise the SAME code path and any measured
# difference is noise. Kept as a historical harness only.
#
# Detailed turbo mode test with verification

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
RESULTS_DIR="$BENCHMARK_DIR/turbo_detailed_$TIMESTAMP"

mkdir -p "$RESULTS_DIR"

echo -e "${BLUE}=== DTail Turbo Mode Detailed Test ===${NC}"
echo ""

# First, let's verify turbo mode is actually being detected
echo -e "${YELLOW}Testing turbo mode detection...${NC}"

# Create test files
TEST_FILE="$RESULTS_DIR/test_file.log"
for i in {1..10000}; do
    echo "Line $i: This is a test log line with some data" >> "$TEST_FILE"
done

echo -e "${GREEN}✓ Test file created (10k lines)${NC}"
echo ""

# Test 1: Run dcat with turbo disabled (turbo is on by default, so disable it)
echo -e "${YELLOW}Test 1: DCat without turbo mode${NC}"
export DTAIL_TURBOBOOST_DISABLE=yes
cd "$PROJECT_ROOT"
{ time ./dcat "$TEST_FILE" > "$RESULTS_DIR/output_non_turbo.txt" 2>&1; } 2> "$RESULTS_DIR/time_non_turbo.txt"
echo -e "${GREEN}✓ Non-turbo run completed${NC}"

# Test 2: Run dcat with turbo enabled (default behaviour once disable flag is cleared)
echo -e "${YELLOW}Test 2: DCat with turbo mode${NC}"
unset DTAIL_TURBOBOOST_DISABLE
{ time ./dcat "$TEST_FILE" > "$RESULTS_DIR/output_turbo.txt" 2>&1; } 2> "$RESULTS_DIR/time_turbo.txt"
echo -e "${GREEN}✓ Turbo run completed${NC}"
echo ""

# Compare outputs
echo -e "${YELLOW}Verifying outputs match...${NC}"
if diff -q "$RESULTS_DIR/output_non_turbo.txt" "$RESULTS_DIR/output_turbo.txt" > /dev/null; then
    echo -e "${GREEN}✓ Outputs match (turbo mode preserves correctness)${NC}"
else
    echo -e "${RED}✗ Outputs differ! Turbo mode may have issues${NC}"
    diff "$RESULTS_DIR/output_non_turbo.txt" "$RESULTS_DIR/output_turbo.txt" | head -20
fi
echo ""

# Display timing results
echo -e "${BLUE}Timing Results:${NC}"
echo -e "${YELLOW}Non-Turbo:${NC}"
cat "$RESULTS_DIR/time_non_turbo.txt"
echo ""
echo -e "${YELLOW}Turbo:${NC}"
cat "$RESULTS_DIR/time_turbo.txt"
echo ""

# Run focused benchmarks with server mode
echo -e "${BLUE}Running server mode benchmarks...${NC}"

# Start dserver in background
echo -e "${YELLOW}Starting dserver...${NC}"
./dserver -port 12222 > "$RESULTS_DIR/dserver.log" 2>&1 &
DSERVER_PID=$!
sleep 2

# Function to run server mode test
run_server_test() {
    local mode=$1
    local output_prefix=$2
    
    # Turbo boost is on by default; the non-turbo arm sets DTAIL_TURBOBOOST_DISABLE.
    if [ "$mode" == "turbo" ]; then
        unset DTAIL_TURBOBOOST_DISABLE
    else
        export DTAIL_TURBOBOOST_DISABLE=yes
    fi
    
    echo -e "${YELLOW}Running $mode mode server test...${NC}"
    { time ./dcat --server localhost:12222 "$TEST_FILE" > "$RESULTS_DIR/${output_prefix}_server_output.txt" 2>&1; } 2> "$RESULTS_DIR/${output_prefix}_server_time.txt"
}

# Run server tests
run_server_test "non-turbo" "non_turbo"
run_server_test "turbo" "turbo"

# Kill dserver
kill $DSERVER_PID 2>/dev/null || true
wait $DSERVER_PID 2>/dev/null || true

echo ""
echo -e "${BLUE}Server Mode Timing Results:${NC}"
echo -e "${YELLOW}Non-Turbo (server):${NC}"
cat "$RESULTS_DIR/non_turbo_server_time.txt"
echo ""
echo -e "${YELLOW}Turbo (server):${NC}"
cat "$RESULTS_DIR/turbo_server_time.txt"
echo ""

# Generate summary report
REPORT="$RESULTS_DIR/summary.md"
echo "# Turbo Mode Detailed Test Results" > "$REPORT"
echo "" >> "$REPORT"
echo "**Timestamp:** $TIMESTAMP" >> "$REPORT"
echo "**Test File:** 10,000 lines" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Direct Mode (Serverless)" >> "$REPORT"
echo "" >> "$REPORT"
echo "### Non-Turbo:" >> "$REPORT"
echo '```' >> "$REPORT"
cat "$RESULTS_DIR/time_non_turbo.txt" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

echo "### Turbo:" >> "$REPORT"
echo '```' >> "$REPORT"
cat "$RESULTS_DIR/time_turbo.txt" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

echo "## Server Mode" >> "$REPORT"
echo "" >> "$REPORT"
echo "### Non-Turbo:" >> "$REPORT"
echo '```' >> "$REPORT"
cat "$RESULTS_DIR/non_turbo_server_time.txt" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

echo "### Turbo:" >> "$REPORT"
echo '```' >> "$REPORT"
cat "$RESULTS_DIR/turbo_server_time.txt" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

echo "## Notes" >> "$REPORT"
echo "" >> "$REPORT"
echo "- Turbo mode is expected to show improvement primarily in server mode" >> "$REPORT"
echo "- The benefits are more pronounced with larger files and network operations" >> "$REPORT"
echo "- Direct (serverless) mode may show less improvement due to already optimized paths" >> "$REPORT"

echo -e "${GREEN}✓ Detailed test completed${NC}"
echo -e "${GREEN}Results saved to: $RESULTS_DIR${NC}"
echo -e "${GREEN}Summary: $REPORT${NC}"

# Show the report
echo ""
cat "$REPORT"