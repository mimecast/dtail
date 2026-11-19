# Integration Tests Refactoring Guide

> **Status**: This guide was originally written as a forward-looking plan. That
> plan has since been executed: `integrationtests/testhelpers.go` now contains
> exactly the helpers described here (`skipIfNotIntegrationTest`,
> `NewTestServer`, `NewCommandArgs`, `runDualModeTest`, `TestFileSet`,
> `cleanupFiles`, `runCommandAndVerify`). This document is kept as **backlog**
> for the *remaining* work: several older test files (e.g. `dcat_test.go`,
> `dgrep_test.go`, `dserver_test.go`, the `djournal*` tests, `dtailhealth_test.go`)
> still use the old `startCommand` pattern and have not been migrated to the
> helpers yet. Newly written tests should use the helpers from
> `integrationtests/testhelpers.go` directly.

## Overview

This guide outlines the refactoring opportunities for the dtail integration tests to reduce code duplication and improve maintainability. It documents the target patterns (as implemented in `testhelpers.go`) and the patterns still found in the not-yet-migrated test files.

## Key Benefits of Refactoring

1. **Reduced Code Duplication**: ~40-50% reduction in test code
2. **Improved Maintainability**: Changes to common patterns only need to be made in one place
3. **Better Test Hygiene**: Automatic cleanup using `t.Cleanup()`
4. **Clearer Test Intent**: Helper functions make tests more readable
5. **Reduced Copy-Paste Errors**: Less boilerplate to copy incorrectly

## Common Patterns Identified

### 1. Test Skip Pattern
**Before:**
```go
if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
    t.Log("Skipping")
    return
}
```

**After:**
```go
skipIfNotIntegrationTest(t)
```

### 2. Server Setup Pattern
**Before:**
```go
port := getUniquePortNumber()
bindAddress := "localhost"
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

_, _, _, err := startCommand(ctx, t,
    "", "../dserver",
    "--cfg", "none",
    "--logger", "stdout",
    "--logLevel", "error",
    "--bindAddress", bindAddress,
    "--port", fmt.Sprintf("%d", port),
)
if err != nil {
    t.Error(err)
    return
}
time.Sleep(500 * time.Millisecond)
```

**After:**
```go
server := NewTestServer(t)
if err := server.Start("error"); err != nil {
    t.Error(err)
    return
}
```

### 3. File Cleanup Pattern
**Before:**
```go
defer os.Remove(outFile)
defer os.Remove(csvFile)
defer os.Remove(queryFile)
```

**After:**
```go
cleanupFiles(t, outFile, csvFile, queryFile)
// or
fileSet := &TestFileSet{...}
fileSet.Cleanup(t)
```

### 4. Command Arguments Pattern
**Before:**
```go
args := []string{
    "--plain", "--cfg", "none",
    "--servers", fmt.Sprintf("%s:%d", bindAddress, port),
    "--trustAllHosts", "--noColor",
    "--files", inFile,
}
```

**After:**
```go
args := NewCommandArgs()
args.Plain = true
args.Servers = []string{server.Address()}
args.TrustAllHosts = true
args.NoColor = true
args.Files = []string{inFile}
// args.ToSlice() produces the string array
```

### 5. Dual Mode Testing Pattern
**Before:**
```go
func TestX(t *testing.T) {
    if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
        t.Log("Skipping")
        return
    }
    
    t.Run("Serverless", func(t *testing.T) {
        testXServerless(t)
    })
    
    t.Run("ServerMode", func(t *testing.T) {
        testXWithServer(t)
    })
}
```

**After:**
```go
func TestX(t *testing.T) {
    runDualModeTest(t, DualModeTest{
        Name: "TestX",
        ServerlessTest: testXServerless,
        ServerTest: testXWithServer,
    })
}
```

## Refactoring Strategy

### Phase 1: Add Helper Functions (complete)
1. `testhelpers.go` is merged with all the common utilities listed above
2. All tests still pass

### Phase 2: Refactor Test by Test (remaining work)
1. Start with simpler tests (e.g., `dcat_test.go`, `dgrep_test.go`, then the
   `dserver*`, `djournal*` and `dtailhealth` tests)
2. Refactor one test function at a time
3. Run tests after each refactoring
4. Commit after each file is complete

### Phase 3: Additional Improvements
1. Add table-driven tests where appropriate
2. Create test fixtures for common scenarios
3. Add more sophisticated helpers as patterns emerge

## Example Refactoring Results

### Before (TestDCat1WithServer):
```go
func testDCat1WithServer(t *testing.T, inFile string) error {
    outFile := "dcat1.out"
    port := getUniquePortNumber()
    bindAddress := "localhost"

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    _, _, _, err := startCommand(ctx, t,
        "", "../dserver",
        "--cfg", "none",
        "--logger", "stdout",
        "--logLevel", "error",
        "--bindAddress", bindAddress,
        "--port", fmt.Sprintf("%d", port),
    )
    if err != nil {
        return err
    }

    time.Sleep(500 * time.Millisecond)

    _, err = runCommand(ctx, t, outFile,
        "../dcat", "--plain", "--cfg", "none",
        "--servers", fmt.Sprintf("%s:%d", bindAddress, port),
        "--files", inFile,
        "--trustAllHosts",
        "--noColor")
    if err != nil {
        return err
    }

    cancel()

    if err := compareFiles(t, outFile, inFile); err != nil {
        return err
    }

    os.Remove(outFile)
    return nil
}
```

### After:
```go
func testDCat1WithServer_Refactored(t *testing.T, inFile string) {
    fileSet := &TestFileSet{
        InputFile:    inFile,
        OutputFile:   "dcat1.out",
        ExpectedFile: inFile,
    }
    fileSet.Cleanup(t)

    server := NewTestServer(t)
    if err := server.Start("error"); err != nil {
        t.Error(err)
        return
    }

    args := NewCommandArgs()
    args.Plain = true
    args.Servers = []string{server.Address()}
    args.Files = []string{inFile}
    args.TrustAllHosts = true
    args.NoColor = true

    err := runCommandAndVerify(t, server.ctx, fileSet.OutputFile, fileSet.ExpectedFile,
        "../dcat", args.ToSlice()...)
    if err != nil {
        t.Error(err)
    }
}
```

## Metrics

Based on the examples:
- **Lines of code reduction**: ~45%
- **Boilerplate elimination**: ~70%
- **Improved readability**: Subjective but significant
- **Error-prone patterns eliminated**: Port management, cleanup, context handling

## Next Steps

1. ~~Review and approve the helper functions~~ (done: `testhelpers.go` is merged)
2. ~~Create a PR with `testhelpers.go`~~ (done)
3. Incrementally migrate the remaining test files that still use the old
   `startCommand` pattern — e.g. `dcat_test.go`, `dgrep_test.go`,
   `dserver_test.go` (and the other `dserver_*` tests), the `djournal*` tests,
   and `dtailhealth_test.go` — one test function (or file) at a time, running
   the integration tests after each migration
4. Document any new patterns that emerge
5. Consider creating a test generator for common scenarios