#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now exercise the SAME code path and any measured
# difference is noise. Kept as a historical harness only.
#
# Final turbo mode performance test

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
RED='\033[0;31m'
NC='\033[0m'

echo -e "${BLUE}=== Final Turbo Mode Performance Test ===${NC}"
echo ""

# Create test file
TEST_FILE="final_test_data.log"
echo -e "${YELLOW}Creating test file (1M lines)...${NC}"
rm -f "$TEST_FILE"
for i in {1..1000000}; do
    echo "2025-01-01 10:00:00 INFO [app] Processing request ID=$i status=OK latency=42ms" >> "$TEST_FILE"
done
echo -e "${GREEN}✓ Test file created${NC}"
echo ""

# Test 1: Non-turbo mode
echo -e "${YELLOW}Testing non-turbo mode...${NC}"
export DTAIL_TURBOBOOST_DISABLE=yes
start=$(date +%s.%N)
../dcat --plain "$TEST_FILE" > /dev/null 2>&1
end=$(date +%s.%N)
non_turbo_time=$(echo "$end - $start" | bc)
echo -e "${GREEN}Non-turbo time: ${non_turbo_time}s${NC}"

# Test 2: Turbo mode
echo -e "${YELLOW}Testing turbo mode...${NC}"
unset DTAIL_TURBOBOOST_DISABLE
start=$(date +%s.%N)
../dcat --plain "$TEST_FILE" > /dev/null 2>&1
end=$(date +%s.%N)
turbo_time=$(echo "$end - $start" | bc)
echo -e "${GREEN}Turbo time: ${turbo_time}s${NC}"

# Calculate improvement
improvement=$(echo "scale=2; (($non_turbo_time - $turbo_time) / $non_turbo_time) * 100" | bc)
speedup=$(echo "scale=2; $non_turbo_time / $turbo_time" | bc)

echo ""
echo -e "${BLUE}=== Results ===${NC}"
echo "Non-turbo: ${non_turbo_time}s"
echo "Turbo:     ${turbo_time}s"
if (( $(echo "$improvement > 0" | bc -l) )); then
    echo -e "${GREEN}Turbo mode is ${speedup}x faster (${improvement}% improvement)${NC}"
else
    echo -e "${RED}Turbo mode is slower by ${improvement}%${NC}"
fi

# Test with colors too
echo ""
echo -e "${YELLOW}Testing with colors enabled...${NC}"

# Non-turbo with colors
export DTAIL_TURBOBOOST_DISABLE=yes
start=$(date +%s.%N)
../dcat "$TEST_FILE" > /dev/null 2>&1
end=$(date +%s.%N)
non_turbo_color_time=$(echo "$end - $start" | bc)

# Turbo with colors
unset DTAIL_TURBOBOOST_DISABLE
start=$(date +%s.%N)
../dcat "$TEST_FILE" > /dev/null 2>&1
end=$(date +%s.%N)
turbo_color_time=$(echo "$end - $start" | bc)

echo "Non-turbo (colors): ${non_turbo_color_time}s"
echo "Turbo (colors):     ${turbo_color_time}s"

# Cleanup
rm -f "$TEST_FILE"

echo ""
echo -e "${BLUE}Test complete!${NC}"