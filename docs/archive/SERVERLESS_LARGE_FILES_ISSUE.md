# Serverless Mode Large File Issue

> **Status: OUTDATED / ARCHIVED (2026-09).** This issue description is kept for
> historical reference only. The channel-based serverless fix it describes no
> longer exists (the channel-less read/output path is now the only mode), and
> its central claim — that files larger than ~100 KB time out in serverless
> mode — is disproved by the 2026-09-23 upstream benchmark, which measured a
> serverless cat of a 1 GiB file completing in 1.46 s with output correctness
> verified. See `docs/upstream-benchmark-2026-09-23.md` for the current
> measured serverless and SSH-server performance.

## Summary
While the serverless mode deadlock has been partially resolved, files larger than approximately 10KB still experience timeouts in serverless mode.

## Current Status
- ✅ Files up to 10KB work correctly
- ❌ Files larger than 100KB timeout
- ❌ The 72MB test_data.log used in profiling examples still hangs

## Technical Details
The current fix uses a channel-based approach to prevent deadlocks:
- Separate goroutines for reading from client/server handlers
- Buffered channels (100 slots) for data transfer
- 32KB buffer size for read operations

However, this approach still has limitations with larger files, possibly due to:
1. Channel buffer exhaustion
2. Synchronization issues between read/write operations
3. EOF handling complexities
4. Memory pressure from buffering large amounts of data

## Workaround
For profiling large files, avoid serverless mode by specifying a dummy server:
```bash
./dcat -profile -profiledir profiles -plain -cfg none -servers dummy test_data.log
```

## Proposed Solutions

### Short-term
1. Increase channel buffer sizes dynamically based on file size
2. Implement backpressure handling
3. Add proper flow control between readers and writers

### Long-term
1. Redesign serverless mode to avoid bidirectional copying
2. Implement a proper streaming architecture
3. Consider using io.Pipe with proper goroutine management
4. Add file size detection and automatic mode switching

## Testing
Use the test_serverless.go script to verify fixes:
```go
// Test different file sizes
sizes := []struct {
    name string
    size int
}{
    {"tiny", 100},        // ✅ Works
    {"small", 1024},      // ✅ Works  
    {"medium", 10240},    // ✅ Works
    {"large", 102400},    // ❌ Timeouts
    {"xlarge", 1048576},  // ❌ Timeouts
}
```

## Impact
- Profiling benchmarks work for small to medium test files
- Large file profiling requires non-serverless mode
- Integration tests may need adjustment if they use large files in serverless mode