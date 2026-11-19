package loggers

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type gatedLogWriter struct {
	countingWriter
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *gatedLogWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.countingWriter.Write(data)
}

func TestStdoutFlushDeadlineIsNotResetByMoreWrites(t *testing.T) {
	for _, beforeStart := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			w := &countingWriter{}
			s := newStdoutWriter(w)
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			defer func() { cancel(); wg.Wait() }()
			if beforeStart {
				s.Raw("first")
			}
			wg.Add(1)
			s.Start(ctx, &wg)
			if !beforeStart {
				s.Raw("first")
			}
			synctest.Wait()
			time.Sleep(stdoutIdleFlushInterval - time.Nanosecond)
			s.RawBytes([]byte("second"))
			synctest.Wait()
			if w.String() != "" {
				t.Fatal("unexpected early flush")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if got := w.String(); got != "firstsecond" {
				t.Fatalf("later write postponed the flush: %q", got)
			}
			// Exercise rearming a used timer after a long quiet period.
			time.Sleep(time.Hour)
			s.Log("third")
			synctest.Wait()
			time.Sleep(stdoutIdleFlushInterval)
			synctest.Wait()
			if got := w.String(); got != "firstsecondthird\n" {
				t.Fatalf("write after idle was stranded: %q", got)
			}
		})
	}
}

func TestStdoutExplicitFlushAndPauseKeepLaterWriteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &countingWriter{}
		s := newStdoutWriter(w)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		s.Start(ctx, &wg)
		defer func() { cancel(); wg.Wait() }()
		s.Raw("one")
		synctest.Wait()
		s.Flush()
		s.Pause()
		time.Sleep(stdoutIdleFlushInterval / 2)
		s.Resume()
		s.RawWithColors("plain", "color")
		synctest.Wait()
		time.Sleep(stdoutIdleFlushInterval / 2)
		synctest.Wait()
		if got := w.String(); got != "onecolor" {
			t.Fatalf("explicit flush/pause lost the outstanding deadline: %q", got)
		}
		cancel()
		wg.Wait()
		// As before, post-shutdown stdout remains usable with explicit Flush.
		s.Raw("after-stop")
		s.Flush()
		if got := w.String(); got != "onecolorafter-stop" {
			t.Fatalf("post-shutdown synchronous flush: %q", got)
		}
	})
}

func TestStdoutRemainingStarterStillFlushes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &countingWriter{}
		s := newStdoutWriter(w)
		first, stopFirst := context.WithCancel(context.Background())
		second, stopSecond := context.WithCancel(context.Background())
		var wgFirst, wgSecond sync.WaitGroup
		defer func() { stopFirst(); stopSecond(); wgFirst.Wait(); wgSecond.Wait() }()
		wgFirst.Add(1)
		s.Start(first, &wgFirst)
		s.Raw("first")
		synctest.Wait() // The first worker owns the pending timer.
		wgSecond.Add(1)
		s.Start(second, &wgSecond)
		synctest.Wait()
		stopFirst()
		wgFirst.Wait()
		s.Raw("second")
		synctest.Wait()
		time.Sleep(stdoutIdleFlushInterval)
		synctest.Wait()
		if got := w.String(); got != "firstsecond" {
			t.Fatalf("stopped worker stranded the remaining starter: %q", got)
		}
	})
}

func TestStdoutCanRestartAfterShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &countingWriter{}
		s := newStdoutWriter(w)
		for _, message := range []string{"first", "second"} {
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Add(1)
			s.Start(ctx, &wg)
			s.Raw(message)
			synctest.Wait()
			cancel() // Cancel with a pending timer, before it fires.
			wg.Wait()
		}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		s.Start(ctx, &wg)
		defer func() { cancel(); wg.Wait() }()
		s.Raw("third")
		synctest.Wait()
		time.Sleep(stdoutIdleFlushInterval)
		synctest.Wait()
		if got := w.String(); got != "firstsecondthird" {
			t.Fatalf("restart failed to rearm the idle flush: %q", got)
		}
	})
}

func TestLoggerIdleRearmKeepsPeriodicPhase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &countingWriter{}
		s := newStdoutWriter(w)
		dir := t.TempDir()
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "phase"}, dir)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		s.Start(ctx, &wg)
		f.Start(ctx, &wg)
		defer func() { cancel(); wg.Wait() }()
		synctest.Wait()
		for _, message := range []string{"first", "second"} {
			// Both writes arrive 75ms after the most recent periodic tick;
			// a fresh100ms delay would miss the original next25ms deadline.
			time.Sleep(75 * time.Millisecond)
			s.Raw(message)
			f.Raw(message)
			synctest.Wait()
			time.Sleep(25 * time.Millisecond)
			synctest.Wait()
			want := "first"
			if message == "second" {
				want += "second"
			}
			if w.String() != want || readLogFile(t, dir, "phase") != want {
				t.Fatal("idle rearm added a full interval instead of retaining the tick phase")
			}
			time.Sleep(time.Second) // Let the file timer go idle before rearming.
			synctest.Wait()
		}
	})
}

func TestFileLoggerSleepsWhileIdleAndRefreshesOnFirstWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		before, after, beforeDay, afterDay := midnightTimes()
		clock := newFakeClock(before)
		f := newFile(Strategy{Rotation: DailyRotation}, dir)
		f.clock = clock.now
		stop := startFileLogger(t, f)
		defer stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := clock.reads.Load(); got != 0 {
			t.Fatalf("unused logger read the clock %d times", got)
		}
		f.Log("before")
		synctest.Wait()
		time.Sleep(fileIdleFlushInterval)
		synctest.Wait()
		if got := readLogFile(t, dir, beforeDay); got != "before\n" {
			t.Fatalf("sparse write missed flush deadline: %q", got)
		}
		time.Sleep(2 * fileIdleFlushInterval)
		synctest.Wait()
		reads := clock.reads.Load()
		clock.set(after)
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := clock.reads.Load(); got != reads {
			t.Fatalf("quiet logger kept waking: clock reads %d -> %d", reads, got)
		}
		f.Log("after")
		f.Flush()
		if got := readLogFile(t, dir, afterDay); got != "after\n" {
			t.Fatalf("first write after idle chose stale day: %q", got)
		}
		if got := readLogFile(t, dir, beforeDay); got != "before\n" {
			t.Fatalf("new-day output leaked into old file: %q", got)
		}
	})
}

func TestFileLoggerRefreshesDayDuringAutoFlushedTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		before, after, beforeDay, afterDay := midnightTimes()
		clock := newFakeClock(before)
		f := newFile(Strategy{Rotation: DailyRotation}, dir)
		f.clock = clock.now
		stop := startFileLogger(t, f)
		defer stop()
		bulk := strings.Repeat("x", 2*fileWriterBufSize)
		f.Raw(bulk)
		synctest.Wait()
		if got := f.writer.Buffered(); got != 0 {
			t.Fatalf("test needs an auto-flushed bulk write, buffered=%d", got)
		}
		clock.set(after)
		time.Sleep(fileIdleFlushInterval)
		synctest.Wait()
		f.Raw("new-day")
		f.Flush()
		if got := readLogFile(t, dir, beforeDay); got != bulk {
			t.Fatal("bulk output changed or crossed the rotation boundary")
		}
		if got := readLogFile(t, dir, afterDay); got != "new-day" {
			t.Fatalf("auto-flush disabled day refresh: %q", got)
		}
	})
}

func TestFileLoggerRotateAfterIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "signal"}, dir)
		stop := startFileLogger(t, f)
		defer stop()
		f.Log("old")
		f.Flush()
		time.Sleep(time.Second)
		synctest.Wait()
		if err := os.Rename(filepath.Join(dir, "signal.log"), filepath.Join(dir, "old.log")); err != nil {
			t.Fatal(err)
		}
		f.Rotate()
		synctest.Wait()
		f.Log("new")
		f.Flush()
		if readLogFile(t, dir, "old") != "old\n" || readLogFile(t, dir, "signal") != "new\n" {
			t.Fatal("idle Rotate did not reopen the file")
		}
	})
}

func TestIdleFlushErrorsAndStoppedFileFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fail := failingLogWriter{err: errors.New("flush failed")}
		s := newStdoutWriter(fail)
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "error"}, "")
		f.lastFileName = "error"
		f.writer = bufio.NewWriterSize(fail, fileWriterBufSize)
		diagnostics := &countingWriter{}
		f.errorWriter = diagnostics
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		s.Start(ctx, &wg)
		f.Start(ctx, &wg)
		defer func() { cancel(); wg.Wait() }()
		s.Raw("stdout")
		f.Raw("file")
		synctest.Wait()
		time.Sleep(fileIdleFlushInterval)
		synctest.Wait()
		if !strings.Contains(diagnostics.String(), "flush idle log output: flush failed") {
			t.Fatalf("flush error lost: %q", diagnostics.String())
		}
		// Sticky writer errors must not strand the next producer or shutdown.
		s.Raw("again")
		f.Raw("again")
		cancel()
		wg.Wait()
		start := time.Now()
		f.Flush()
		if elapsed := time.Since(start); elapsed != fileFlushTimeout {
			t.Fatalf("post-shutdown Flush bound = %v, want %v", elapsed, fileFlushTimeout)
		}
	})
}

func TestFileLoggerBlockedFlushPreservesBackpressureAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &gatedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(w.release) })
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "blocked"}, "")
		f.queue = newFileQueue(2)
		f.lastFileName = "blocked"
		f.writer = bufio.NewWriterSize(w, fileWriterBufSize)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		f.Start(ctx, &wg)
		defer func() { release(); cancel(); wg.Wait() }()
		f.Raw("first")
		synctest.Wait()
		time.Sleep(fileIdleFlushInterval)
		synctest.Wait()
		select {
		case <-w.entered:
		default:
			t.Fatal("timer did not flush into the blocked sink")
		}
		queued := strings.Repeat("q", fileWriterBufSize)
		f.Raw(queued)
		f.Raw(queued)
		done := make(chan struct{})
		go func() { f.Log(strings.Repeat("b", fileWriterBufSize)); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("full queue failed to apply backpressure")
		default:
		}
		cancel()
		release()
		wg.Wait()
		<-done
		if got := w.String(); got != "first"+queued+queued+strings.Repeat("b", fileWriterBufSize)+"\n" {
			t.Fatalf("shutdown dropped/reordered queued output (%d bytes)", len(got))
		}
	})
}

func TestStdoutWriteRacingBlockedTimerFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &gatedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(w.release) })
		s := newStdoutWriter(w)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		s.Start(ctx, &wg)
		defer func() { release(); cancel(); wg.Wait() }()
		s.Raw("first")
		synctest.Wait()
		time.Sleep(stdoutIdleFlushInterval)
		synctest.Wait()
		select {
		case <-w.entered:
		default:
			t.Fatal("timer did not flush into the blocked sink")
		}
		done := make(chan struct{})
		go func() { s.Raw("second"); close(done) }()
		release()
		<-done
		cancel()
		wg.Wait()
		if got := w.String(); got != "firstsecond" {
			t.Fatalf("flush boundary lost/reordered output: %q", got)
		}
	})
}
