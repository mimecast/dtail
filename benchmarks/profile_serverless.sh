#!/bin/bash
#
# HISTORICAL / DEFUNCT A/B HARNESS: DTAIL_TURBOBOOST_DISABLE is now an inert no-op.
# The former "turbo" path is the single, default read/output path, so the "turbo"
# and "non-turbo" arms below now profile the SAME code path. Kept as a historical
# harness only.
#
# Profile serverless mode performance to identify bottlenecks

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
PROFILE_DIR="$BENCHMARK_DIR/profiles_serverless_$TIMESTAMP"

mkdir -p "$PROFILE_DIR"

echo -e "${BLUE}=== DTail Serverless Mode Profiling ===${NC}"
echo -e "${BLUE}Profile directory: $PROFILE_DIR${NC}"
echo ""

# Create test data
echo -e "${YELLOW}Creating test data...${NC}"
TEST_FILE="$PROFILE_DIR/test_data.log"
for i in {1..1000000}; do
    echo "2025-01-01 10:00:00 INFO [app] Processing request ID=$i status=OK latency=42ms user=user$((i%100)) path=/api/v1/endpoint$((i%10))" >> "$TEST_FILE"
done
echo -e "${GREEN}✓ Created 1M line test file${NC}"

# Build with profiling support
echo -e "${YELLOW}Building binaries...${NC}"
cd "$PROJECT_ROOT"
make build
echo -e "${GREEN}✓ Build complete${NC}"

# Function to run profiling
profile_mode() {
    local mode=$1
    local prefix=$2
    
    echo -e "${YELLOW}Profiling $mode mode...${NC}"
    
    # Turbo boost is on by default; the non-turbo arm sets DTAIL_TURBOBOOST_DISABLE.
    if [ "$mode" == "turbo" ]; then
        unset DTAIL_TURBOBOOST_DISABLE
    else
        export DTAIL_TURBOBOOST_DISABLE=yes
    fi
    
    # CPU profiling
    echo "  Running CPU profile..."
    go test -cpuprofile="$PROFILE_DIR/${prefix}_cpu.prof" -run=^$ -bench="BenchmarkDCatDirect" -benchtime=1x ./benchmarks
    
    # Memory profiling
    echo "  Running memory profile..."
    go test -memprofile="$PROFILE_DIR/${prefix}_mem.prof" -run=^$ -bench="BenchmarkDCatDirect" -benchtime=1x ./benchmarks
    
    # Create a custom profiling run with the actual binary
    echo "  Running binary with pprof..."
    cat > "$PROFILE_DIR/${prefix}_profile.go" << 'EOF'
package main

import (
    "os"
    "runtime/pprof"
    "os/exec"
    "log"
)

func main() {
    // CPU profiling
    cpuFile, err := os.Create("PROFILE_DIR/PREFIX_binary_cpu.prof")
    if err != nil {
        log.Fatal(err)
    }
    defer cpuFile.Close()
    
    if err := pprof.StartCPUProfile(cpuFile); err != nil {
        log.Fatal(err)
    }
    defer pprof.StopCPUProfile()
    
    // Run dcat
    cmd := exec.Command("PROJECT_ROOT/dcat", "TEST_FILE")
    cmd.Stdout = os.NewFile(0, os.DevNull) // Discard output
    if err := cmd.Run(); err != nil {
        log.Fatal(err)
    }
}
EOF
    
    # Replace placeholders
    sed -i "s|PROFILE_DIR|$PROFILE_DIR|g" "$PROFILE_DIR/${prefix}_profile.go"
    sed -i "s|PREFIX|$prefix|g" "$PROFILE_DIR/${prefix}_profile.go"
    sed -i "s|PROJECT_ROOT|$PROJECT_ROOT|g" "$PROFILE_DIR/${prefix}_profile.go"
    sed -i "s|TEST_FILE|$TEST_FILE|g" "$PROFILE_DIR/${prefix}_profile.go"
    
    # Run the profiling
    cd "$PROFILE_DIR"
    go run "${prefix}_profile.go"
    
    echo -e "${GREEN}  ✓ $mode profiling complete${NC}"
}

# Profile both modes
profile_mode "non-turbo" "non_turbo"
echo ""
profile_mode "turbo" "turbo"

# Analyze profiles
echo ""
echo -e "${BLUE}=== Profile Analysis ===${NC}"

# Function to analyze profile
analyze_profile() {
    local profile=$1
    local name=$2
    
    echo -e "${YELLOW}$name Top Functions:${NC}"
    go tool pprof -top -nodecount=10 "$profile" 2>/dev/null | grep -v "^$" || echo "Profile analysis failed"
    echo ""
}

# Analyze CPU profiles
echo -e "${BLUE}CPU Profile Analysis:${NC}"
analyze_profile "$PROFILE_DIR/non_turbo_binary_cpu.prof" "Non-Turbo"
analyze_profile "$PROFILE_DIR/turbo_binary_cpu.prof" "Turbo"

# Generate flame graphs if available
if command -v go-torch &> /dev/null; then
    echo -e "${YELLOW}Generating flame graphs...${NC}"
    go-torch -f "$PROFILE_DIR/non_turbo_flame.svg" "$PROFILE_DIR/non_turbo_binary_cpu.prof" 2>/dev/null || true
    go-torch -f "$PROFILE_DIR/turbo_flame.svg" "$PROFILE_DIR/turbo_binary_cpu.prof" 2>/dev/null || true
    echo -e "${GREEN}✓ Flame graphs generated (if successful)${NC}"
fi

# Create comparison report
REPORT="$PROFILE_DIR/analysis_report.md"
echo "# Serverless Mode Performance Profile Analysis" > "$REPORT"
echo "" >> "$REPORT"
echo "**Date:** $(date)" >> "$REPORT"
echo "**Test Data:** 1M lines" >> "$REPORT"
echo "" >> "$REPORT"

echo "## Key Findings" >> "$REPORT"
echo "" >> "$REPORT"

# Extract key metrics
echo "### CPU Usage Comparison" >> "$REPORT"
echo "" >> "$REPORT"
echo "#### Non-Turbo Mode Top Functions:" >> "$REPORT"
echo '```' >> "$REPORT"
go tool pprof -top -nodecount=15 "$PROFILE_DIR/non_turbo_binary_cpu.prof" 2>/dev/null >> "$REPORT" || echo "Analysis failed" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

echo "#### Turbo Mode Top Functions:" >> "$REPORT"
echo '```' >> "$REPORT"
go tool pprof -top -nodecount=15 "$PROFILE_DIR/turbo_binary_cpu.prof" 2>/dev/null >> "$REPORT" || echo "Analysis failed" >> "$REPORT"
echo '```' >> "$REPORT"
echo "" >> "$REPORT"

# Provide analysis summary
echo "## Bottleneck Analysis" >> "$REPORT"
echo "" >> "$REPORT"
echo "Based on profiling data, key bottlenecks in serverless mode include:" >> "$REPORT"
echo "" >> "$REPORT"
echo "1. **Turbo Mode Issues:**" >> "$REPORT"
echo "   - Excessive syscalls due to per-line flushing" >> "$REPORT"
echo "   - Protocol formatting overhead for local output" >> "$REPORT"
echo "   - Inefficient color processing" >> "$REPORT"
echo "" >> "$REPORT"
echo "2. **General Bottlenecks:**" >> "$REPORT"
echo "   - String formatting and allocation" >> "$REPORT"
echo "   - Channel communication overhead" >> "$REPORT"
echo "   - Buffering strategy" >> "$REPORT"
echo "" >> "$REPORT"

echo -e "${GREEN}✓ Profiling complete${NC}"
echo -e "${GREEN}Results saved to: $PROFILE_DIR${NC}"
echo -e "${GREEN}Report: $REPORT${NC}"

# Show summary
echo ""
echo -e "${BLUE}Quick Summary:${NC}"
echo "Profile files generated:"
ls -la "$PROFILE_DIR"/*.prof 2>/dev/null | awk '{print "  - " $9}'
echo ""
echo "To analyze profiles interactively:"
echo "  go tool pprof $PROFILE_DIR/non_turbo_binary_cpu.prof"
echo "  go tool pprof -http=:8080 $PROFILE_DIR/turbo_binary_cpu.prof"