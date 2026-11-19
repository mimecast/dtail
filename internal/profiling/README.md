# DTail Internal Profiling Package

This package provides built-in profiling capabilities for DTail commands (dcat, dgrep, dmap), enabling performance analysis and optimization.

## Overview

The profiling package integrates Go's pprof profiling tools directly into DTail commands, allowing users to:
- Profile CPU usage to identify performance bottlenecks
- Track memory allocations to optimize memory usage
- Generate heap profiles to detect memory leaks
- Analyze performance without external profiling tools

## Architecture

### Core Components

1. **Profiler Interface** (`profiler.go`)
   - Manages profiling lifecycle (start, stop, write)
   - Handles multiple profile types concurrently
   - Provides thread-safe operations

2. **Profile Manager** (`manager.go`)
   - Coordinates CPU, memory, and allocation profiling
   - Generates timestamped profile filenames
   - Manages profile output directories

3. **Metrics Collection** (`metrics.go`)
   - Captures runtime metrics (goroutines, memory stats, GC info)
   - Provides before/after snapshots
   - Formats metrics for logging

## Usage

### Command-Line Flags

All DTail commands support these profiling flags:

```bash
# Enable both CPU and memory profiling
dcat -profile -profiledir profiles file.log

# Enable only CPU profiling
dgrep -cpuprofile -profiledir profiles -regex "error" file.log

# Enable only memory profiling
dmap -memprofile -profiledir profiles -query "select count(*)" -files data.csv
```

### Programmatic Usage

```go
import "github.com/mimecast/dtail/internal/profiling"

func main() {
    // Create profiler
    profiler := profiling.NewProfiler("dcat", "profiles")
    
    // Start profiling
    if err := profiler.Start(); err != nil {
        log.Fatal(err)
    }
    defer profiler.Stop()
    
    // Your application code here
    processFiles()
    
    // Profiler automatically writes profiles on Stop()
}
```

### Advanced Features

#### Runtime Metrics

The profiler captures detailed runtime metrics:

```go
metrics := profiling.GetRuntimeMetrics()
// Includes: NumGoroutines, Alloc, TotalAlloc, Sys, NumGC, GCPauseTotal
```

#### Custom Profile Points

Add profiling snapshots at specific points:

```go
profiler.LogMetrics("before_processing")
// ... heavy processing ...
profiler.LogMetrics("after_processing")
```

## Profile Output

### File Naming Convention

Profiles are saved with descriptive filenames:
```
<tool>_<type>_<timestamp>.prof

Examples:
- dcat_cpu_20240626_143022.prof
- dgrep_mem_20240626_143025.prof
- dmap_alloc_20240626_143028.prof
```

### Profile Types

1. **CPU Profile** (`*_cpu_*.prof`)
   - Samples CPU usage during execution
   - Shows time spent in each function
   - Helps identify computational bottlenecks

2. **Memory Profile** (`*_mem_*.prof`)
   - Snapshot of heap allocations at program end
   - Shows current memory usage by function
   - Useful for finding memory leaks

3. **Allocation Profile** (`*_alloc_*.prof`)
   - Tracks all allocations during execution
   - More detailed than memory profile
   - Helps reduce allocation pressure

## Analysis

### Using pprof

Analyze profiles with Go's pprof tool:

```bash
# Interactive mode
go tool pprof profiles/dcat_cpu_20240626_143022.prof

# Web interface (requires graphviz)
go tool pprof -http=:8080 profiles/dcat_cpu_20240626_143022.prof

# Generate flame graph
go tool pprof -svg profiles/dgrep_cpu_*.prof > profile.svg

# Top functions by CPU usage
go tool pprof -top profiles/dmap_cpu_*.prof
```

### Using dtail-tools

DTail provides a convenient tool for profile analysis:

```bash
# List all profiles
dtail-tools profile -mode list

# Analyze specific profile
dtail-tools profile -mode analyze profiles/dcat_cpu_*.prof

# Open web interface
dtail-tools profile -mode analyze profiles/dcat_cpu_*.prof -web
```

## Implementation Details

### Thread Safety

All profiling operations are thread-safe using sync.Mutex to ensure:
- Safe concurrent access to profiler state
- Atomic start/stop operations
- Protected file writes

### Error Handling

The profiler handles errors gracefully:
- Non-blocking: profiling errors don't stop the main program
- Logged warnings for profile write failures
- Automatic cleanup on errors

### Performance Impact

Profiling overhead is minimal:
- CPU profiling: ~2-5% overhead
- Memory profiling: Negligible (snapshot at end)
- Allocation profiling: ~5-10% overhead

## Best Practices

### When to Profile

1. **Development Phase**
   - Profile new features before merging
   - Establish performance baselines
   - Identify optimization opportunities

2. **Performance Issues**
   - User reports of slow operations
   - High resource usage in production
   - Unexplained performance degradation

3. **Regular Maintenance**
   - Periodic profiling to catch regressions
   - Before/after optimization work
   - Release performance validation

### Profiling Strategies

1. **Start Simple**
   ```bash
   # Quick CPU profile
   dcat -cpuprofile -profiledir profiles large_file.log
   ```

2. **Memory Issues**
   ```bash
   # Check for memory leaks
   dgrep -memprofile -profiledir profiles -regex "pattern" huge_file.log
   ```

3. **Full Analysis**
   ```bash
   # Complete profiling suite
   dmap -profile -profiledir profiles -query "complex query" -files data.csv
   ```

### Common Patterns to Look For

#### CPU Profiles
- Functions with high Flat% (direct CPU usage)
- High cumulative% indicating expensive call chains
- Unexpected hotspots in utility functions

#### Memory Profiles
- Large allocations in unexpected places
- Many small allocations (consider pooling)
- Growing heap over time (potential leaks)

## Integration with CI/CD

### Automated Profiling

Add profiling to your CI pipeline:

```yaml
# GitHub Actions example
- name: Run Profiled Tests
  run: |
    # Run with profiling enabled
    go test -bench=. -cpuprofile=cpu.prof -memprofile=mem.prof ./...
    
    # Analyze results
    go tool pprof -top cpu.prof > cpu_analysis.txt
    
    # Check for regressions
    ./scripts/check_performance.sh cpu_analysis.txt
```

### Performance Gates

Set thresholds for key metrics:
```bash
# Example: Fail if function takes >20% CPU
if go tool pprof -top cpu.prof | grep "myFunction" | awk '{print $2}' | grep -E "[2-9][0-9]\.|100\."; then
    echo "Performance regression detected!"
    exit 1
fi
```

## Troubleshooting

### No Profiles Generated

1. Check write permissions for profile directory
2. Ensure command completes successfully
3. Verify profiling flags are correct
4. Check disk space availability

### Empty Profiles

1. Increase workload size
2. Ensure program runs long enough
3. Check for early exits or errors
4. Verify profile type matches workload

### Analysis Errors

1. Ensure profile format is valid
2. Check Go version compatibility
3. Install graphviz for visualizations
4. Use matching pprof version

## Contributing

When modifying the profiling package:

1. **Maintain Compatibility**
   - Don't break existing command-line flags
   - Preserve profile output format
   - Keep thread-safety guarantees

2. **Add Tests**
   - Unit tests for new functionality
   - Integration tests with actual profiles
   - Benchmark impact of changes

3. **Document Changes**
   - Update this README
   - Add code comments
   - Include examples

## Future Enhancements

Planned improvements:
- Block profiling support
- Mutex contention profiling
- Custom profile aggregation
- Real-time profiling dashboard
- Profile comparison tools
- Automatic anomaly detection

## References

- [Go Profiling Guide](https://go.dev/blog/pprof)
- [pprof Documentation](https://github.com/google/pprof/blob/master/doc/README.md)
- [Runtime Package](https://pkg.go.dev/runtime)
- [Testing Package Profiling](https://pkg.go.dev/testing#hdr-Benchmarks)