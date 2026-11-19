# DTail Channel-less Read/Output Path (formerly "Turbo Boost")

## Overview

This document describes DTail's channel-less read/output path. It was originally
introduced as an opt-out "turbo boost" optimization, but it is now the single,
default processing path for all read/output operations. It improves performance
by using channel-less processing and optimized I/O. The on/off toggles described
in early revisions of this document (`DTAIL_TURBOBOOST_DISABLE`,
`DTAIL_CHANNELLESS_GREP`, `DTAIL_OPTIMIZED_READER`) have been removed; the
channel-less path is now unconditional.

## Problem Statement

The original dgrep implementation used multiple channels in a pipeline:
- `rawLines chan *bytes.Buffer` (buffer: 100) - Raw lines read from file
- `lines` channel (buffer: 100) - Allocated line objects to send to client

This created several performance issues:
1. Fixed channel buffer sizes causing blocking under high throughput
2. Context switching overhead between goroutines
3. Channel synchronization overhead
4. Memory allocations for channel operations

## Solution

The channel-less implementation replaces the channel pipeline with direct function calls using the `line.Processor` interface.

### Key Components

1. **line.Processor Interface** (`internal/io/line/processor.go`)
   - Defines methods for processing lines without channels
   - `ProcessLine()` - Handle a single line
   - `Flush()` - Ensure buffered data is written
   - `Close()` - Clean up resources

2. **DirectLineProcessor** (`internal/server/handlers/line_writer.go`)
   - Implements `line.Processor` for direct read operations
   - Delegates formatted output to the active direct or network writer
   - Flushes the writer at read boundaries

3. **File Reading** (`internal/io/fs/readfile_processor_optimized.go`)
   - `Start()` - Channel-less file reading
   - Direct callbacks instead of channel sends
   - Inline regex filtering without goroutines
   - Uses buffered line reading instead of byte-by-byte
   - Custom scanner with a pooled buffer and 1 MiB token limit
   - Efficient handling of long lines
   - Special optimization for tail mode

### Feature Flags (historical — removed)

Early revisions gated this work behind opt-in environment variables
(`DTAIL_CHANNELLESS_GREP`, `DTAIL_OPTIMIZED_READER`). These no longer exist: the
channel-less, optimized read path is always on and cannot be toggled.

### Benefits

1. **Reduced Latency**: No channel queuing delays
2. **Lower Memory Usage**: No channel buffers
3. **Better CPU Efficiency**: Fewer context switches
4. **Simpler Code Flow**: Direct processing without goroutine coordination
5. **Predictable Performance**: No channel blocking

### Compatibility

- The original channel-based implementation has since been removed; the
  channel-less path is the only one.
- Same command-line interface
- Protocol compatibility maintained
- All integration tests pass

### Performance Testing

Use the maintained benchmark targets to measure the current read/output path:

```bash
make benchmark-quick
make benchmark
```

The historical channel-based and byte-at-a-time implementations have been
removed, so there is no runtime toggle or legacy implementation to compare.

### Usage

The channel-less path is always active; no environment variables are needed:

```bash
# Run dgrep normally — the channel-less, optimized path is used automatically
dgrep -regex "pattern" file.log
```

### Future Improvements

1. Evaluate configurable reader buffer sizes with representative workloads
2. Add performance metrics collection
3. Consider using io_uring on Linux for async I/O

## Summary

The channel-less path is always on — there is no enable/disable switch. The
former `DTAIL_TURBOBOOST_DISABLE` environment variable and the
`Server.TurboBoostDisable` config field have been removed;
`DTAIL_TURBOBOOST_DISABLE` is now inert and an old config still carrying a
`TurboBoostDisable` key is silently ignored. The example schema
`examples/dtail.schema.json` does not list the removed `Turbo*` keys, so
schema validation flags them as stale, while dtail itself still ignores them.

The path provides:
- Channel-less processing for read operations
- A 1 MiB scanner token limit for non-follow reads
- A pooled 64 KiB buffer for follow reads
- Buffer pooling to reduce memory allocations
