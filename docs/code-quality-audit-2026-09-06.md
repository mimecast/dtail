# DTail code-quality audit, 2026-09-06

> **Historical snapshot, not a live issue tracker.** This audit describes the
> repository at commit `3004a71`. Findings may have been addressed since then;
> verify the current code and tests before treating any item below as open.

Full-repository audit of all production Go code under `cmd/` and `internal/`
(about 26k lines, 290 files) at commit `3004a71`. Five independent passes were
run: defect sweep (find-code-bugs), Go best practices, 100 Go Mistakes, SOLID,
and system-level architecture. No source files were modified by the audit.

Every finding below was verified by reading the code; bug findings marked
**[reproduced]** were additionally confirmed empirically with a throwaway
dserver, race-enabled builds, and goroutine dumps.

## How to use this document for re-verification

Each finding has an **ask task ID**, a **location**, and a **Verify** line
describing what a reviewer should check to decide whether the finding is
resolved. To re-audit later:

1. `ask list` / `ask completed since:this.month` to see which tasks claim to be done.
2. For each finding, run or inspect the **Verify** item at the stated location
   (line numbers are as of `3004a71` and will drift; search by symbol name).
3. Re-run the guardrails: `make clean && make build`, `go vet ./...`,
   `gofmt -l .`, `errcheck ./...`, `go test -race ./...`,
   `DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test`.
4. When everything is confirmed, mark gate task **i3** done and move the
   `audit/<date>` git tag to HEAD (audit-tagging skill).

## Test policy for every task

All unit tests and all integration tests (`make clean && make build`, then
`DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test`) must pass **without
modifying anything under `integrationtests/` or its fixtures**. Integration
tests must not be edited, skipped, weakened or deleted to make a fix pass. If
a fixer believes an integration test itself is wrong and should be corrected,
they must stop, annotate the task with the test name, the reason, and the
proposed change, and ask the user for permission before touching it. Every
task carries this policy as an annotation. A re-verification pass should
check `git log --stat -- integrationtests/` for unexplained changes.

## Summary

| Category | HIGH | MEDIUM | LOW |
|---|---|---|---|
| Bugs / defects | 4 | 5 | 0 |
| SOLID | 3 | 10 | 7 |
| Architecture | 3 | 10 | 5 |
| Go Best Practices (incl. 100 Go Mistakes) | 7 | 21 | 19 |
| **Total** | **17** | **46** | **31** |

Tasks created: 10 `+bugfix` (f2 g2 h2 i2 j2 k2 l2 m2 n2 t2), 29 `+codequality`,
1 `+audit` closure gate (**i3**) depending on all 39.

---

## Part 1: Confirmed bugs (`+bugfix`)

### f2 [HIGH] Teardown deadlock leaks follow-mode MapReduce sessions [reproduced]

- **Location:** `internal/server/handlers/basehandler.go:115-122` (`baseHandler.Shutdown`), `internal/mapr/server/aggregate.go:147-155` (`Aggregate.Shutdown`), `internal/server/handlers/readcommand.go:442-475`, `internal/io/fs/readfile_processor_optimized.go:845-989`, `internal/server/server.go:279-319`.
- **Symptom:** dserver leaks a stuck session (goroutines, open fd or journalctl child, aggregate) forever when a client running a MapReduce query in follow mode (`dtail --query`, `journal:` follow, Continuous jobs) disconnects.
- **Mechanism:** `baseHandler.Shutdown` calls `Aggregate.Shutdown()` (which does `processorsWg.Wait()`) before `h.done.Shutdown()`. The tail read goroutine owning the `AggregateProcessor` only exits on ctx cancel, and that cancel is driven by `h.done`. Circular wait. All terminate goroutines in `handleShellRequest` block in `handler.Shutdown()`, `sshConn.Close()` is never reached.
- **Evidence:** after `kill -9` of `dtail --query "from STATS select count($line) interval 1"`, dserver goroutine dump showed goroutines parked in `Aggregate.Shutdown aggregate.go:150 <- baseHandler.Shutdown:119 <- handleShellRequest.func1 server.go:280` plus the live `tailWithProcessorOptimized`, still present after 60 s.
- **Verify:** `baseHandler.Shutdown` cancels command contexts (or calls `Abort`) before waiting on processors, or the wait is bounded. Repeat the kill -9 scenario and check `/debug/pprof/goroutine` on dserver shows no `Aggregate.Shutdown` parked goroutine after a few seconds. A regression test should exist that builds a handler with a tail read feeding an aggregate and asserts `Shutdown()` returns.

### g2 [HIGH] Data race on the per-server GroupSet in the client [reproduced]

- **Location:** `internal/clients/handlers/maprhandler.go:930-994` (`MaprHandler.Write`), `internal/mapr/client/aggregate.go:41-118`, `internal/clients/connectors/serverconnection.go:356-357`.
- **Symptom:** dmap/dtail client races on `client.Aggregate.group` on cancellation; possible fatal "concurrent map read and map write".
- **Mechanism:** `MaprHandler.Write` (io.Copy goroutine) mutates the group set while `ServerConnection.handle` calls `handler.Shutdown() -> Flush()` from the connection goroutine after `ctx.Done()`, with the SSH session still open; no mutex.
- **Evidence:** `-race` build reported `DATA RACE` (read at `globalgroupset.go:50` via `Flush`, write at `groupset.go:46` via `Aggregate`) and exited 66 in 4 of 6 SIGTERM-mid-stream runs.
- **Verify:** a mutex (or equivalent) guards `Aggregate()`/`Flush()` in `internal/mapr/client/aggregate.go`, or `Flush` runs only after the copy goroutine has exited. Run a race-enabled dmap against a server and SIGTERM it mid-stream several times; no race report.

### h2 [HIGH] AUTHKEY fast path bypasses AllowFrom for pseudo-users [reproduced]

- **Location:** `internal/server/handlers/serverhandler.go:207-240` (`handleAuthKeyCommand`), `internal/ssh/server/publickeycallback.go:184-198`, `internal/server/scheduler.go:289`, `internal/server/continuous.go:530`.
- **Symptom:** permission-bypassing pseudo-users (`DTAIL-SCHEDULE`, `DTAIL-CONTINUOUS`) can authenticate by public key from any IP, bypassing `AllowFrom`.
- **Mechanism:** `handleAuthKeyCommand` stores any key under the authenticated username; `publicKeyCallback` honours the store for any username from any address; `ValidateReadTarget` skips all permission checks for the pseudo-users, whose only guards are the job-name password and `AllowFrom`. Internal scheduler/continuous clients run with `NoAuthKey=false`, so every job run auto-registers the dserver owner's `~/.ssh/id_rsa.pub` under those unrestricted identities for 24 h.
- **Verify:** AUTHKEY is refused (and the store is not consulted) for the pseudo-users; internal clients set `NoAuthKey=true`; optionally keys are bound to the remote IP. Test: run a scheduled job, then attempt a public-key login as `DTAIL-SCHEDULE` from a non-AllowFrom address; it must fail.

### i2 [HIGH] Serverless client never exits after context cancellation [reproduced]

- **Location:** `internal/clients/connectors/serverless.go:113-118, 133-155, 241-243`, `internal/server/handlers/basehandler.go:167`.
- **Symptom:** `--shutdownAfter`, `--timeout`, Ctrl+C do not stop a serverless client; dmap final result never written.
- **Mechanism:** on `ctx.Done`, `Serverless.handle` shuts down only the in-process server handler; the goroutine blocked in the client handler's `Read` (selects only on `commands`/`Done`) never wakes, `ioWg.Wait()` blocks, `Start` never returns.
- **Evidence:** `dtail -cfg none -plain -shutdownAfter 2 -files x.log` still running after 60 s; dump shows `Serverless.handle serverless.go:243` and `baseHandler.Read basehandler.go:167`.
- **Verify:** the client handler is shut down on cancel too (or the reader selects on ctx). Run the same command; it must exit within a second or two of the deadline and dmap must print its final result.

### j2 [MEDIUM] Signals swallowed after context cancellation [reproduced]

- **Location:** `internal/io/signal/signal.go:15-88`.
- **Symptom:** after ctx cancel, SIGTERM/SIGINT/SIGHUP/SIGQUIT are ignored; process needs SIGKILL.
- **Mechanism:** both `InterruptCh*` goroutines return on `ctx.Done` without `signal.Stop`, leaving Notify registered so default termination is suppressed.
- **Verify:** `defer signal.Stop(...)` (or equivalent) present; a hung client responds to `timeout`'s SIGTERM.

### k2 [MEDIUM] stdout logger `Pause()` needs a concurrent log call [reproduced]

- **Location:** `internal/io/dlog/loggers/stdout.go:92-128`, `internal/io/prompt/prompt.go`.
- **Symptom:** with `--quiet`/`--plain` against an unknown host the host-key prompt is never printed; client hangs silently.
- **Mechanism:** `Pause()` sends on an unbuffered channel whose only receiver is a non-blocking select inside `log()`; `prompt.Ask` pauses before printing.
- **Verify:** pause is state-based (flag + mutex/cond). Connect with `--quiet` to an unknown host; prompt appears.

### l2 [MEDIUM] Host-key prompt busy-loops on stdin EOF [reproduced]

- **Location:** `internal/io/prompt/prompt.go:58-79`.
- **Symptom:** 1,175,502 prompts in 8 s with stdin from `/dev/null`; pegged CPU, flooded stdout.
- **Mechanism:** `Ask` ignores `ReadString` errors and re-prints forever.
- **Verify:** EOF/error is treated as "no" and returns. Run a client with `</dev/null` against an unknown host; it exits promptly with one prompt.

### m2 [MEDIUM] Aggregate serialization loop blocks on its own channel

- **Location:** `internal/mapr/server/aggregate.go:355-383` (`serializationLoop`), `finalizeWhenIdle`.
- **Symptom:** up to 60 s stall of serialization and dmap completion; session held open.
- **Mechanism:** `serializationLoop` calls `Serialize()` on each tick, which blocks sending to `a.serialize` (cap 1) that only this loop drains; a pending token from `finalizeWhenIdle` plus a coincident tick stalls the loop.
- **Verify:** ticks call the serialize implementation directly or send non-blockingly; grep confirms the loop is no longer both the sole receiver and a blocking sender.

### n2 [MEDIUM] Follow mode drops lines written between truncation and reopen [reproduced]

- **Location:** `internal/io/fs/readfile.go:105-118, 131-199`, `internal/server/handlers/readcommand.go:418-440`.
- **Symptom:** a line appended immediately after `: > x.log` was never shown; a line 7 s later was.
- **Mechanism:** truncation is detected only every 3 s, retried after 2 s, and every reopen seeks to EOF (`TailFile.seekEOF=true`).
- **Verify:** after a detected truncation/rotation the file is reopened at offset 0 (tail -F semantics), only the first open seeks to EOF. Repeat the truncate-then-append test; the line appears.

### t2 [MEDIUM] dtailhealth pprof shutdown skipped; `os.Exit` bypasses defers (from Go audit)

- **Location:** `cmd/dtailhealth/main.go:46-72`, `cmd/dserver/main.go:59,68,121`, `cmd/dtailhealth/main.go:58,76`.
- **Symptom:** `pprofServer, pprofErr := cli.NewPProfServer(pprof)` at line 48 shadows the outer `pprofServer` (line 46), so the graceful shutdown block never runs. `os.Exit` after `defer cancel()` skips the deferred cancels. `status := 0` at line 58 is dead.
- **Verify:** `go vet -vettool=$(which shadow) ./cmd/...` clean; mains use `func main(){ os.Exit(run()) }` or equivalent so defers run.

### Dropped (investigated, not a bug)

- `internal/server/handlers/readcommand.go:630` `os.Stdin.Stat()` nil deref with closed stdin: Go runtime reopens fds 0-2 on `/dev/null`, unreachable (tested).

---

## Part 2: Design, architecture and convention findings (`+codequality`)

Grouped by task. Each task lists the audit(s) that raised it.

### o2 [HIGH] Lint tooling, gofmt, Makefile targets (Go BP, 100GM #16, Arch)

- 21 files not gofmt-clean (11 production): `internal/server/handlers/line_writer.go:374,417`, `internal/config/env.go:19,25`, `cmd/dtail-tools/main.go`, `internal/io/fs/readfile.go:200`, `internal/profiling/profiler.go`, `internal/profiling/flags.go`, `internal/io/line/processor.go`, `internal/io/pool/scanner_pool.go`, `internal/io/dlog/loggers/file.go`, `internal/tools/pgo/pgo.go`, `internal/tools/benchmark/benchmark.go`, `internal/tools/common/utils.go`.
- `Makefile:43-70`: `lint` installs archived `golang.org/x/lint/golint`; `vet` ends with `grep -R TODO:` (fails when the last TODO is removed, scans `.git`); `test` shells `find | while read` instead of `go test ./...`. No `.golangci.yml`.
- Mechanical lint hits to fold in: `internal/server/stats.go:135` timer allocated per loop (use Ticker), `basehandler.go:541` `deadline.Sub(time.Now())` -> `time.Until`, `readcommand.go:169` redundant return, `config/env.go:7` Yoda condition, 34 `WriteString(fmt.Sprintf(...))` -> `fmt.Fprintf`, 10 `//comment` without space.
- **Verify:** `gofmt -l .` prints nothing; `.golangci.yml` exists enabling gofmt, errcheck, staticcheck, errorlint, govet shadow; `make lint` runs golangci-lint; `make vet` is `go vet ./...`; `make test` is `go test -race ./...` (ideally `-shuffle=on`).

### p2 [HIGH] 73 ignored errors, including closes on written files (Go BP, 100GM #53/#54)

- Written-file cases: `internal/mapr/groupsetresult.go:125,160,213` (CSV outfile `defer fd.Close()` then rename: a close failure is lost while the `.tmp` is still renamed into place), `internal/profiling/profiler.go:84,101,118,139,167` (pprof files), `internal/io/dlog/loggers/file.go:231-232` (Flush/Close), `internal/config/initializer.go:96` (`os.Setenv`), `internal/tools/pgo/pgo.go:1172` (`cmd.Run()`).
- About 41 of the 73 are `defer x.Close()` on read handles (acceptable if made explicit).
- **Verify:** `errcheck ./...` clean or only explicit `_ =` on read handles; `groupsetresult.go` closes before rename and returns the error.

### q2 [HIGH] Dependency hygiene (Go BP)

- `go.mod:8-9` `golang.org/x/crypto v0.39.0` (SSH implementation) vs v0.56.0 current; `x/sys`, `x/term` behind; `golang.org/x/term` declared direct but unused (`go mod tidy -diff` removes it).
- **Verify:** `go list -m -u all` shows current x/ modules; `go mod tidy -diff` empty; `govulncheck ./...` clean.

### r2 [HIGH] Panics on config/user/flag input (Go BP, 100GM #48, Arch POLA)

- 18 `panic(...)` sites turning recoverable conditions into stack traces: `internal/config/config.go:52,55`, `internal/config/initializer.go:205`, `internal/user/name.go:11,14,17,25`, `internal/io/dlog/level.go:55` (unknown LogLevel), `internal/io/dlog/loggers/factory.go:38` (unknown logger name).
- Constructors that return `error` yet panic: `internal/clients/baseclient.go:73` (invalid regex), `internal/clients/catclient.go:53,57` and grep/tail/mapr/health equivalents (bad session spec), `internal/clients/connectors/serverconnection.go:155` (unparseable port).
- Per-connection `dlog.Server.FatalPanic` kills the daemon for all clients: `internal/server/handlers/serverhandler.go:54,58,85`, `healthhandler.go:49`, `internal/mapr/server/aggregate.go:121`.
- `internal/io/signal/signal.go:33-47` exits 0 on forced termination (should be non-zero).
- Legitimate programmer-error panic to keep: `publickeycallback.go:29`.
- **Verify:** `grep -rn 'panic(' internal/config internal/user internal/io/dlog internal/clients` shows none of the above; a typo in `dtail.json` yields a one-line error and non-zero exit, no stack trace.

### s2 [HIGH] File logger panics and Logger contract (100GM #48, SOLID LSP/ISP)

- `internal/io/dlog/loggers/file.go:219,226` `getWriter` panics on `MkdirAll`/`OpenFile` failure and runs on the logger goroutine started at `file.go:99`: an unwritable log dir (EACCES, read-only FS, midnight rotation) crashes dserver or the client.
- `file.go:150-160` `RawWithColors` panics ("Colors not supported in file logger"); `LogWithColors` routes to it; callers special-case via `SupportsColors` (`dlog.go:192,232,318`, `fout.go:67-72`).
- `loggers/logger.go:10-21` `Logger` has 10 methods; `Rotate` only meaningful for `file` (`stdout.go:138` stub, `none` all stubs); `Pause/Resume` only for terminal sinks. `payloadFileTeer` (`dlog.go:204`) already shows the optional-capability pattern.
- `loggers/factory.go:25-39` string switch panics on unknown name, caches inconsistently (`none` never stored).
- **Verify:** no `panic(` in `internal/io/dlog/loggers/`; running dserver with an unwritable `LogDir` logs to stderr and keeps serving; `file.RawWithColors` writes the plain message; `Logger` interface is 6 methods with optional `Rotator`/`Pauser`.

### u2 [MEDIUM] Error hygiene (Go BP, 100GM #49/#51, staticcheck ST1005)

- `io.EOF` compared with `==`/`!=`: `internal/clients/connectors/serverless.go:150,188`, `internal/io/fs/readfile_processor.go:155`, `internal/io/fs/readfile_processor_optimized.go:115` (line 141 deliberately documents why it uses `==`; keep), `internal/server/server.go:315`.
- `internal/mapr/logformat/parser.go:110` uses `%v` not `%w` (the only non-wrapping `Errorf`; 140 `%w` uses elsewhere).
- `parser.go:104-113` returns a usable parser together with a non-nil error; `internal/mapr/query.go:69` `NewQuery("")` returns `nil, nil`.
- 40 capitalised error strings: `internal/mapr/query.go:156-236` (13), `internal/config/initializer.go:60-89`, exported sentinels `logformat/parser.go:15` `ErrIgnoreFields`, `logformat/mimecast.go:8`.
- **Verify:** `golangci-lint run --enable-only errorlint,staticcheck` clean for these; `NewQuery("")` returns an error or empty Query.

### v2 [MEDIUM] Mechanical Go cleanups (Go BP, 100GM #1/#2/#14/#96/#100)

- Mixed value/pointer receivers: `internal/clients/healthclient.go:44-65`, `internal/io/line/line.go:49,58`, `internal/io/fs/readfile.go:51-131`, `internal/io/dlog/loggers/fout.go:112`.
- Builtin shadowing: `func new` in `internal/io/dlog/dlog.go:49` and `internal/regex/regex.go:62`; variable `cap` in `internal/server/handlers/readcommand.go:154`; package-shadowing locals `user := user.New` (`internal/server/server.go:205,330`), `regex := regex.New` (`internal/clients/baseclient.go:71`); `err` re-declared in inner scopes 24x (`internal/io/fs/validatedreadtarget.go:104`, `readfile_processor_optimized.go:346,372,387`).
- unparam: `readcommand.go:173` `retryInterval` unused, `mapcommand.go:24` `argc`, `internal/mapr/groupsetresult.go:41,90,176` always-nil results, all five `makeSessionSpec` always-nil error (`catclient.go:46`).
- `interface{}` 33x vs `any` (`dlog.go:108`, `readcommand_server.go:12` `LogContext() interface{}`); `readcommand.go:317,335` if-else chains that should be `switch`.
- `internal/io/pool/scanner_pool.go:40-48,58-65` zero the entire 1 MB / 64 KB buffer on every Put (per file read via `readfile_processor_optimized.go:48,308`); bufio.Scanner overwrites anyway.
- `runtime.NumCPU()` for throttle sizes ignores cgroup limits: `catclient.go:29`, `grepclient.go:30`, `maprclient.go:66`, `tailclient.go:24`, `healthclient.go:31`, `loggers/file.go:70`; prefer `runtime.GOMAXPROCS(0)`.
- **Verify:** `golangci-lint run --enable-only unparam,predeclared,govet` clean; zeroing loop gone from `scanner_pool.go`.

### w2 [MEDIUM] Package documentation and naming (Go BP)

- 50 of 52 packages lack a `// Package xxx ...` comment (only `internal/color/color.go` and `internal/io/journal/reader.go` have one).
- Stutter: `lcontext.LContext`, `dlog.DLog`, `protocol.ProtocolCompat`, `discovery.Discovery`, `prompt.Prompt`, `regex.Regex`, `server.Server`.
- Package names colliding with stdlib/x: `internal/io/fs`, `internal/io/signal`, `internal/user`, `internal/ssh` (forcing `gosignal`/`gossh` aliases). Two packages named `handlers` both export a different `Handler` interface (`internal/server/handlers`, `internal/clients/handlers`).
- **Verify:** `go list ./... | xargs -n1 go doc` shows a package sentence for each.

### x2 [MEDIUM] Test coverage and testing conventions (Go BP, 100GM #84/#86/#90)

- Coverage: `internal/server` 29.6%, `internal/clients` 35.2%, `internal/config` 36.0%, `internal/ssh/client` 36.9%, `internal/io/dlog` 9.7%, `internal/session` 20.0%.
- 20 packages with no tests: all `cmd/*` except `cmd/dtail`, `internal/io/{line,pool,prompt,signal}`, `internal/{protocol,source,user,version,options,omode}`.
- Only 31 of 83 test files table-driven; 0 fuzz tests despite parsers `internal/mapr/token.go`, `internal/mapr/query.go`, `internal/mapr/logformat/*`; 18 `time.Sleep` in unit tests (`internal/io/dlog/loggers/stdout_test.go:169-189`, `internal/mapr/server/aggregate_test.go:252,359`, `internal/server/handlers/commandcancel_test.go:79-140`); no `-shuffle`.
- **Verify:** `go test -cover ./internal/...` shows the listed packages at or above 60%; `Fuzz*` functions exist in `internal/mapr`.

### y2 [MEDIUM] Legacy `StartWithProcessor` read path (Go BP, SOLID ISP, Arch YAGNI)

- `internal/io/fs/filereader.go:15-22` `FileReader` requires both `StartWithProcessor` and `StartWithProcessorOptimized`; only production caller `internal/server/handlers/readcommand.go:449` uses Optimized; legacy byte-at-a-time loop `internal/io/fs/readfile_processor.go:21-185` (file 359 lines) reachable only from tests; `internal/io/journal/reader.go:70-81` implements both identically.
- **Verify:** `grep -rn StartWithProcessor internal/` shows one method (ideally renamed `Start`); `readfile_processor.go` legacy loop deleted, `filteringProcessor` kept.

### z2 [MEDIUM] Dead post-turbo handler code (Arch YAGNI, SOLID ISP/LSP)

- Zero production callers: `internal/server/handlers/line_writer.go:246-370` `ChannelWriter` (file 777 lines); `lineprocessor.go` `GrepLineProcessor`, `HandlerWriter`, `ServerMessageProcessor`; `baseHandler.lines chan *line.Line` (`basehandler.go:56-101`, consumed 205-235, counted in flush at 512, initialised `serverhandler.go:64`, `healthhandler.go:36`) has no producers; `readcommand.go:33-37` `readProcessor` duplicates `line.Processor` (`internal/io/line/processor.go:9-22`); `internal/ssh/client/customkeycallback.go:11-25` `CustomCallback`; `internal/clients/args.go:11` `clients.Args`; `internal/server/stats.go:133-141` `waitForConnections`.
- **Verify:** each symbol gone; `baseHandler.Read` has no `lines` consume branch.

### 03 [HIGH] `aggregateInputGrace` sleep hides a race (100GM #58/#65)

- `internal/server/handlers/shutdown_coordinator.go:75` `maybeFinishAggregateInput` sleeps `aggregateInputGrace` (100 ms, line 16) because "a sibling read command may be dispatched but not yet registered in the pending-files counter". MapReduce final-result correctness depends on 100 ms being enough under load.
- **Verify:** pending files registered synchronously at dispatch (in `baseHandler.dispatchCommand`, before the goroutine starts); the sleep and constant removed; a concurrency regression test exists.

### 13 [MEDIUM] Sleep/poll-based synchronization in flush, Read loop, outputManager (Arch, 100GM #65)

- `basehandler.go:563` 100 ms sleep after `ta.Shutdown()` which is already synchronous (`aggregate.go:147-155`).
- `basehandler.go:509-583` `flush()` polls every 10 ms until unsent==0; after 1/3/5 s logs "Some lines remain unsent" and closes the session (silent truncation for slow clients).
- `output_manager.go:321-347,443-463` `len(chan)` polls with 10 ms sleeps; `shutdown_coordinator.go:92,98` `ShutdownSerializeWait` 500 ms, `ShutdownIdleRecheckWait`.
- `basehandler.go:151-157` Read loop re-arms `time.After(pollInterval)` with `defaultOutputReadRetryInterval = 1ms` (`output_manager.go:15`): every idle session wakes ~1000x/s and allocates a timer each time. Also `internal/ssh/client/knownhostscallback.go:180` timer per iteration.
- **Verify:** `grep -rn 'time.Sleep' internal/server/handlers` shows none in the shutdown/flush path; flush blocks on a done channel with a hard deadline reported to the client as an error; Read wakes on a notification channel/cond.

### 23 [MEDIUM] Retire timing knobs and the 50 ms OutputTransmissionDelay (Arch KISS) — depends on 03, 13

- `readcommand.go:462-469` sleeps `OutputTransmissionDelay` (50 ms, "crucial for integration tests") after every file read.
- `internal/config/server.go` nine knobs exist only to tune sleeps: `OutputTransmissionDelayMs`, `OutputEOFWaitBaseMs`, `OutputEOFWaitPerFileMs`, `OutputEOFWaitMaxMs`, `OutputChannelBufferSize`, `OutputFlushTimeoutMs`, `OutputFlushPollIntervalMs`, `OutputReadRetryIntervalMs`, `OutputEOFAckTimeoutMs`, `ShutdownOutputSerializeWaitMs`.
- **Verify:** the 50 ms delay is gone; knobs without a consumer are removed (or accepted-and-ignored for one release); AGENTS.md "Tuning knobs" section updated.

### 33 [MEDIUM] Context propagation (100GM #60/#61/#62, Go BP)

- Command contexts derived from `context.Background()` instead of the connection ctx: `basehandler.go:307,433-444`, `sessioncommand.go:137,167`; cancellation re-wired via an extra watcher goroutine per command.
- nil ctx tolerated: `internal/cli/runtime.go:25`, `basehandler.go:433`, `line_writer.go:558,641`.
- Not cancellable: `internal/server/server.go:106` `net.Listen`, `:420` `net.LookupIP`, `internal/cli/pprof.go:41`; `internal/mapr/server/aggregate.go:152,223,410` Shutdown/Serialize without ctx; `shutdown_coordinator.go:89`.
- **Verify:** `grep -rn 'context.Background()' internal/server/handlers` shows none in command paths; no per-command watcher goroutine; `net.ListenConfig`/`net.Resolver` used.

### 43 [MEDIUM] Resilience: panic recovery, idle timeout, byte-capped output buffer (Arch)

- `recover()` appears nowhere in production code; a panic in one session (`internal/server/server.go:151,281-310`) terminates dserver for every client.
- After the 10 s handshake deadline (`server.go:22,164-183`) no idle or maximum session timeout; an authenticated idle client holds one of `MaxConnections` (default 10) forever.
- `output_manager.go:11,86-91` buffers 1000 slots of copied ≤64 KB payloads (~64 MB per slow client) before backpressure (`generation_output.go:56-66`, `line_writer.go:413-431`).
- `internal/cli/pprof.go:48` `http.Server` without `ReadHeaderTimeout` (gosec G112); `internal/tools/pgo/pgo.go:881` `http.Get` default client. `readcommand.go:178` `int32(len(paths))` and 7 other unchecked int conversions (G115).
- **Verify:** `defer recover` in connection goroutines; `Server.IdleSessionTimeoutS` (or similar) exists with a default; output buffer bounded by bytes; `ReadHeaderTimeout` set.

### 53 [MEDIUM] `color/brush` reads config globals (SOLID DIP, Arch)

- `internal/color/brush/brush.go:16-110` reads `config.Client.TermColors.*` 75 times; `Colorfy` (used by `dlog.go:196,236,323`, `line_writer.go:114`) nil-derefs unless `config.Setup` ran. `clientOutputFormatter` (`internal/clients/runtime_boundary.go:114-160`) shows the injected alternative.
- **Verify:** `brush.New(theme)` exists; `grep -c 'config\.' internal/color/brush/brush.go` is 0 or near 0.

### 63 [HIGH] Injected Logger interface for leaf packages, part 1 (SOLID DIP, Arch, Go BP)

- `internal/io/dlog/dlog.go:23-32` `dlog.Client/Server/Common` singletons referenced ~363 times across 20 packages; `dlog.new` (49-56) pulls `config.Common`/`config.Hostname()`; panics if started twice (68-95); pulls `color/brush -> config` into the closure of `internal/io/fs`, `internal/io/journal`, `internal/mapr`, `internal/ssh`, `internal/discovery`, `internal/user/server`; nil-guards already at `publickeycallback.go:79,91,97`, `readcommand.go:536`; `TraceEnabled` is nil-receiver-safe as a workaround.
- **Verify:** a small consumer-side `Logger` interface exists in a leaf package; the listed leaf packages take it via constructors and no longer reference `dlog.Common`/`dlog.Server` directly.

### 73 [HIGH] Logger injection into handlers, Aggregate, clients, connectors, part 2 — depends on 63

- Heaviest users: server/handlers 103 refs, server 55, clients 39, connectors 35, ssh/client 25.
- Server package reads client globals: `readcommand.go:518-540` `serverlessOutputWriter` reads `config.Client.LogPayload`, `payloadFileTeeWriter.Write` calls `dlog.Client.RawPayloadFileTee`. Should be an `io.Writer` passed from `clientRuntimeBoundary.NewServerlessHandler`.
- **Verify:** `grep -rn 'dlog\.\(Client\|Server\|Common\)' internal/ | grep -v _test | grep -v '^internal/io/dlog'` returns only composition roots; `internal/server/handlers` has no `config.Client` or `dlog.Client` reference.

### 83 [MEDIUM] Finish RuntimeConfig injection (SOLID DIP, Arch) — depends on 53

- `internal/config/config.go:36-42` globals (89 refs in 6 files); `runtime.go:5-8` `CurrentRuntime()` just snapshots globals; every client constructor calls it (`catclient.go:31`, `tailclient.go:26`, `maprclient.go:68`); `dlog.new` (`dlog.go:50-56`), `fout.go:47`, `dlog.go:192,232,318` (reads `config.Client.TermColorsEnable` even in the server process) go to the globals; `healthhandler.go:28-31` reads `config.Server` while `NewServerHandler` receives `serverCfg` injected; `internal/io/signal/signal.go` imports config only for `InterruptTimeoutS`; `internal/session/spec.go:26` only for `Args`; `initializer.go:96` mutates env via `os.Setenv` during parsing; `internal/ssh/ssh.go:41` `dialAgent`, `ssh/client/authmethods.go:23-26` package-level func vars as test seams.
- **Verify:** `config.Client/Server/Common` referenced only under `cmd/`; `dlog.Start` takes a config struct; `NewHealthHandler(maxFrameSize)`.

### 93 [MEDIUM] `DTAIL_INTEGRATION_TEST_RUN_MODE` branches in production code (Arch SoC/POLA)

- `internal/config/env.go:14-18`, `internal/config/initializer.go:95-96`, `internal/ssh/server/hostkey.go:26`, `internal/ssh/server/publickeycallback.go:136`, `internal/ssh/client/authmethods.go:51,62,96`, `internal/server/handlers/readcommand.go:464`. Setting the variable in a real deployment silently changes hostname and SSH trust file locations.
- **Verify:** `grep -rn DTAIL_INTEGRATION_TEST_RUN_MODE internal/ cmd/` hits at most one mapping site; explicit `HostnameOverride`/`KnownHostsPath`/`AuthorizedKeysPath`/`HostKeyPath` config fields exist.

### a3 [HIGH] Duplicated client flag wiring in `cmd/` (Arch DRY, Go BP thin main)

- `cmd/dcat`, `cmd/dgrep`, `cmd/dmap`, `cmd/dtail` duplicate 21 flag definitions verbatim (104 lines) plus the same bootstrap (`ApplyAuthKeyPathCompatibility`, `config.Setup`, version check, `NewClientRuntime`, Start/Stop/Exit). `cmd/dtail/main.go:23` main is 118 lines, `cmd/dserver/main.go:22` 100 lines. Drift: dmap sets `SSHAgentKeyIndex` twice and exits via `dlog.Client.FatalPanic(err)`; only dtail sets `args.Mode`.
- **Verify:** `cli.BindCommonClientFlags` and `cli.RunClient` (or equivalent) exist; each main under ~40 lines; `-help` output identical to before.

### b3 [MEDIUM] Five clients' identical make* triplets (SOLID OCP) — depends on a3

- `internal/clients/catclient.go:42-60`, `tailclient.go:37-55`, `grepclient.go:43-60`, `healthclient.go:44-60`, `maprclient.go:104-126` identical `makeHandler`/`makeSessionSpec`/`makeCommands`; type assertions `sessionSpecMaker` (`baseclient.go:87`) and `sessionCommitter` (`interactive_control.go:157`); NEXT comment at `maprclient.go:102`.
- **Verify:** one `clientProfile` value (or similar) passed to a single `newBaseClient`; type assertions gone.

### c3 [HIGH] 35-method `readCommandServer` interface; epilogue driven from outside (SOLID ISP, Arch LoD) — depends on z2

- `internal/server/handlers/readcommand_server.go:73-81` union of 7 interfaces (~35 methods); `readCommand` (`readcommand.go:25`) and `shutdownCoordinator` (`shutdown_coordinator.go:19`) both take the union though the coordinator calls ~8; 15 members (170-240) are `ServerConfig -> time.Duration` accessors.
- `readcommand.go:237-275` drives the handler via raw channels (`GetOutputChannel() chan []byte`, `ServerMessagesChannel()`, `CatLimiter() chan struct{}`) and handshake internals (`OutputEpoch`, `SignalOutputEOF`, `WaitForOutputEOFAck`, `PendingAndActive`).
- **Verify:** `readCommandServer` under ten methods; a `readTimings` value struct; a single handler call (e.g. `FinishBatch(ctx)`) owns the EOF/epoch epilogue.

### d3 [HIGH] Split `baseHandler` (SOLID SRP, Go BP function size) — depends on z2, c3

- `basehandler.go:56-101` 22 fields, 31 methods: SSH framing (Read/Write, buffers, frame-size guard), command decode/dispatch (`handleCommand` 109 lines at :138, `dispatchCommand`, `applyCommandTimeout`), once-guarded options, aggregate lifecycle (`atomic.Pointer`), 9-method facade over `outputManager`, close/ack shutdown handshake (509-583), wire formatting inside `Read` (183, 218).
- Other oversized functions: `internal/clients/connectors/serverless.go:104` `handle` 150 lines, `readfile_processor_optimized.go:286` 144, `readcommand.go:172` 111, `mapr/query.go:141` 104 (19 functions over 80 lines).
- **Verify:** `sessionFramer`, `commandDispatcher` (or equivalents) exist; `baseHandler` field/method count roughly halved; wire protocol byte-identical (integration tests).

### e3 [MEDIUM] Wire-format ownership and lineFormatter (SOLID OCP, Arch DRY) — depends on z2

- Formatting policy copy-pasted: `line_writer.go:92-147,173-185,299-304,336-347,451-456`, `lineprocessor.go:74-79,161-173`, `basehandler.go:165-226`; `formatRemoteLine` called from 5 sites with repeated buffer/stat/flush boilerplate (`line_writer.go:146,300,452`, `lineprocessor.go:75`, `basehandler.go:218`).
- `protocol.FieldDelimiter` spliced by hand in 7 packages (`internal/clients/handlers` 7 sites, `internal/color/brush` 12 incl. `SplitN(line, FieldDelimiter, 6)`, `internal/server/handlers` 9, `internal/io/dlog` 5, `internal/mapr` 3, `internal/mapr/logformat` 2); `protocol_codec.go:31-46` owns only part; `ProtocolCompat "4.1"` compared only server-side.
- LSP: `DirectWriter`/`ChannelWriter.WriteServerMessage` silently return nil when serverless (`line_writer.go:166-168,328-330,360`).
- **Verify:** `internal/protocol` exposes Encode/Decode for lines and messages used by both ends; `grep -rn FieldDelimiter internal/ | grep -v internal/protocol` is small; one formatter strategy injected into writers.

### f3 [MEDIUM] Reader factory instead of mode x target switch (SOLID OCP) — depends on y2

- `readcommand.go:315-350` `switch r.mode` x (journal / validated / unvalidated), six branches; four near-identical constructors `NewCatFile`/`NewValidatedCatFile`/`NewTailFile`/`NewValidatedTailFile` (`internal/io/fs/catfile.go`, `tailfile.go`) differing in three bools and an optional target.
- **Verify:** `fs.NewReadFile(ReadOptions)` and a factory keyed by `omode.Mode`; the four constructors gone.

### g3 [MEDIUM] `mapr/server` Aggregate responsibilities (SOLID SRP)

- `internal/mapr/server/aggregate.go:21-73,99-137` 21 fields, 22 methods: parses the query, resolves/constructs the log-format parser (hard-coded generic fallback), calls `config.Hostname()`/`dlog.Server`/`FatalPanic` (121) in the constructor, and owns batching, group-set swap, serialization ticker loop (355-383), shutdown/abort/input-finished lifecycle.
- **Verify:** `NewAggregate(query, parser, hostname, ...)` signature; parser resolved in `newMapCommand`; `batcher`/`serializer` types extracted; no `FatalPanic`.

### h3 [HIGH] Server <-> clients import cycle (Arch loose coupling) — depends on 73, c3

- `internal/server/scheduler.go:11,89`, `internal/server/continuous.go:9,37-38` import `internal/clients`; `internal/clients/runtime_boundary.go:11-13`, `internal/clients/connectors/serverless.go:11` import `internal/server/handlers`, `internal/ssh/server`, `internal/user/server`. `go list -deps ./cmd/dcat` shows `server/handlers`, `mapr/server`, `ssh/server`, `user/server`; dserver links all three client packages. Every client binary runs `exec.LookPath("journalctl")` at package init (`serverhandler.go:44,95-101`). Handler selection on `config.HealthUser` duplicated in `server.go:266-277` and `runtime_boundary.go:71-92`.
- Also LOW: `Serverless.Start` ignores `throttleCh`/`statsCh` that `ServerConnection.Start` uses, so `clients/stats.go:66` reports 0 connected in serverless mode.
- **Verify:** `go list -deps ./cmd/dcat | grep -c internal/ssh/server` is 0; `go list -deps ./internal/server | grep -c internal/clients` is 0; capabilities computed in `server.New`; one `handlers.NewForUser` factory.

---

## Part 3: LOW findings not tracked as tasks

Recorded for completeness; fold into neighbouring tasks opportunistically.

- `internal/config/args.go:17-48` `config.Args` god-struct shared by clients, server, jobs, session specs; split into `ClientArgs`/`ServerArgs`/`ReadSpec`.
- `internal/tools/common/utils.go:14-211` grab-bag utility package (`ParseSize`, `PrintColored`, `GetGitCommit`, `CleanupFiles`, `RunCommandWithTimeout`).
- `internal/io/journal/reader.go:149-150` returns nil after a non-nil `scanErr` when `ctx.Err() != nil` (intentional; add a comment).
- `internal/mapr/logformat/parser.go:37` `init()` registry; `internal/ssh/client/simplecallback.go:35` `PromptAddHosts` stub; `HostKeyCallback` could split into `Wrapper` + optional `UnknownHostPrompter`.
- File layout: constructors after methods in 14 files (`mapr/query.go:67`, `mapr/server/aggregate.go:99`, `line_writer.go:268`); type/var decls after functions in 17 files (`dlog.go:204`, `basehandler.go:56`, `ssh/ssh.go:36`).
- `internal/version` imports `color`; `internal/io/signal` imports `config`.

---

## Part 4: Overall assessment (as of 2026-09-06)

Defect risk concentrates at lifecycle boundaries (handler to aggregate,
connector to handler, logger to prompt, signal handling), not in the hot read
or output path, which held up well under review. Three of the four HIGH bugs
are triggered by ordinary operator actions (client disconnect, Ctrl+C,
deadlines). Structurally the codebase is a layered monolith with good
evolvability seams (capability negotiation, session generations, bounded
reconnects) but the turbo-only migration left dead paths and an over-wide
handler interface, the output/shutdown path substitutes sleeps for completion
signals, and config/logger singletons are half-migrated to injection. The
recommended order is: bugs f2/i2/g2/h2, then dead-code removal (y2, z2) and
cli extraction (a3), then sleep-to-signal (03, 13, 23), then injection
(53, 63, 73, 83), then the engine extraction (h3).
