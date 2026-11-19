package loggers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type partialFileWriter struct {
	file *os.File
	err  error
}

func (w *partialFileWriter) Write(data []byte) (int, error) {
	n, err := w.file.Write(data[:min(7, len(data))])
	if err != nil {
		return n, err
	}
	return n, w.err
}

func TestFileBatchesOwnBorrowedBytesAndPreserveCallOrder(t *testing.T) {
	dir := t.TempDir()
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "batch"}, dir)
	// Queue before starting the worker, then mutate the borrowed input.
	data := []byte("borrowed\n")
	f.RawBytes(data)
	copy(data, "mutated!\n")
	f.Log("diagnostic")
	f.Raw("")
	f.Log("")
	f.RawWithColors("plain", "color")
	f.LogWithColors("diagnostic2", "color2")
	stop := startFileLogger(t, f)
	f.Flush()
	stop()
	if got := readLogFile(t, dir, "batch"); got != "borrowed\ndiagnostic\n\nplaindiagnostic2\n" {
		t.Fatalf("borrowed bytes or mixed call ordering changed: %q", got)
	}
}

func TestFileBatchesKeepConcurrentLargeCallsWhole(t *testing.T) {
	dir := t.TempDir()
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "concurrent"}, dir)
	stop := startFileLogger(t, f)
	const producers = 12
	const size = (fileQueueChunks+3)*fileWriterBufSize + 13
	var wg sync.WaitGroup
	for i := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte('a' + i)}, size)
			f.RawBytes(data)
			clear(data)
		}()
	}
	wg.Wait()
	stop()
	got := readLogFile(t, dir, "concurrent")
	if len(got) != size*producers {
		t.Fatalf("got %d bytes, want %d", len(got), size*producers)
	}
	seen := make(map[byte]bool)
	for i := 0; i < len(got); i += size {
		id := got[i]
		if id < 'a' || id >= 'a'+producers || seen[id] {
			t.Fatalf("missing, duplicate or invalid call at offset %d", i)
		}
		seen[id] = true
		if got[i:i+size] != strings.Repeat(string(id), size) {
			t.Fatalf("concurrent calls interleaved at offset %d", i)
		}
	}
}

func TestFileBatchRotationDoesNotSplitLargeCall(t *testing.T) {
	dir := t.TempDir()
	before, after, beforeDay, afterDay := midnightTimes()
	clock := newFakeClock(before)
	f := newFile(Strategy{Rotation: DailyRotation}, dir)
	f.clock = clock.now
	large := strings.Repeat("x", fileWriterBufSize+13)
	f.Raw(large)
	first := f.queue.take(false)
	if first.end || len(first.data) != fileWriterBufSize {
		t.Fatal("test needs an unfinished multi-chunk call")
	}
	if err := f.writeBatch(first); err != nil {
		t.Fatal(err)
	}
	clock.set(after)
	f.refreshDay()
	f.lastFileName = "" // A consumed signal rotation must not split it either.
	last := f.queue.take(false)
	if !last.end || len(last.data) != 13 {
		t.Fatal("large call has no end marker")
	}
	if err := f.writeBatch(last); err != nil {
		t.Fatal(err)
	}
	f.Log("next-day")
	closeFileLogger(t, f)
	if readLogFile(t, dir, beforeDay) != large || readLogFile(t, dir, afterDay) != "next-day\n" {
		t.Fatal("rotation split a call or did not resume on the next call")
	}
}

func TestFileBatchFailureAbandonsSuffixAcrossRotation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partialWrite%v", partial), func(t *testing.T) {
			dir := t.TempDir()
			before, after, beforeDay, afterDay := midnightTimes()
			clock := newFakeClock(before)
			f := newFile(Strategy{Rotation: DailyRotation}, dir)
			f.clock = clock.now
			var diagnostics bytes.Buffer
			f.errorWriter = &diagnostics
			if partial {
				fd, err := os.Create(filepath.Join(dir, beforeDay+".log"))
				if err != nil {
					t.Fatal(err)
				}
				f.fd = fd
				f.lastFileName = beforeDay
				f.writer = bufio.NewWriterSize(&partialFileWriter{fd, errors.New("partial disk failure")}, fileWriterBufSize)
			} else {
				f.logDir = "" // Writer selection fails, then becomes valid later.
			}
			f.Raw(strings.Repeat("x", fileWriterBufSize+13))
			err := f.writeBatch(f.queue.take(false))
			if err == nil {
				t.Fatal("first chunk must fail")
			}
			f.reportError("write log message", err)
			clock.set(after)
			f.refreshDay()
			f.lastFileName = ""
			f.logDir = dir
			if suffixErr := f.writeBatch(f.queue.take(false)); suffixErr != nil {
				t.Fatalf("discarding the failed call's suffix: %v", suffixErr)
			}
			if got := readLogFile(t, dir, afterDay); got != "" {
				t.Fatalf("failed call resumed in rotated file: %q", got)
			}
			f.Log("next-call")
			closeFileLogger(t, f)
			wantPrefix := ""
			if partial {
				wantPrefix = strings.Repeat("x", 7)
			}
			if readLogFile(t, dir, beforeDay) != wantPrefix || readLogFile(t, dir, afterDay) != "next-call\n" {
				t.Fatal("failure lost its prefix, emitted a suffix, or prevented the next call")
			}
			if !strings.Contains(diagnostics.String(), "write log message: "+err.Error()) {
				t.Fatalf("first-chunk failure not reported: %q", diagnostics.String())
			}
		})
	}
}

func TestFileBatchesSparseDeadlineAndRecycling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &countingWriter{}
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "batch"}, "")
		f.lastFileName = "batch"
		f.writer = bufio.NewWriterSize(w, fileWriterBufSize)
		stop := startFileLogger(t, f)
		defer stop()
		f.Raw("first")
		synctest.Wait()
		time.Sleep(fileIdleFlushInterval - time.Nanosecond)
		f.RawBytes([]byte("second"))
		if w.String() != "" {
			t.Fatal("partial chunk flushed before its deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := w.String(); got != "firstsecond" {
			t.Fatalf("partial chunk missed its original deadline: %q", got)
		}
		f.queue.mu.Lock()
		if len(f.queue.free) != 1 || f.queue.pending != nil || f.queue.size != 0 {
			t.Error("drained chunk not returned for reuse")
		}
		f.queue.mu.Unlock()
	})
}

func TestFileBatchesBoundMemoryAndFinishAcceptedCallOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &gatedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(w.release) })
		f := newFile(Strategy{Rotation: SignalRotation, FileBase: "batch"}, "")
		f.lastFileName = "batch"
		f.writer = bufio.NewWriterSize(w, fileWriterBufSize)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		f.Start(ctx, &wg)
		defer func() { release(); cancel(); wg.Wait() }()
		large := strings.Repeat("q", 20*fileWriterBufSize+7)
		done := make(chan struct{})
		go func() { f.Raw(large); close(done) }()
		<-w.entered
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("oversized call bypassed bounded backpressure")
		default:
		}
		q := f.queue
		q.mu.Lock()
		retained := cap(q.pending)
		for _, chunk := range q.chunks {
			retained += cap(chunk.data)
		}
		for _, buffer := range q.free {
			retained += cap(buffer)
		}
		if q.size != fileQueueChunks || retained > (fileQueueChunks+1)*fileWriterBufSize {
			t.Errorf("queue is not bounded: chunks=%d retained=%d", q.size, retained)
		}
		q.mu.Unlock()
		cancel()
		release()
		wg.Wait()
		<-done
		if got := w.String(); got != large {
			t.Fatalf("shutdown truncated accepted large call: got %d, want %d bytes", len(got), len(large))
		}
	})
}

func TestFileQueueClosedAdmissionAndChunkBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, fileWriterBufSize - 1, fileWriterBufSize, fileWriterBufSize + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			q := newFileQueue(fileQueueChunks)
			message := strings.Repeat("x", size)
			if !q.append("prefix", nil, false) || !q.append(message, nil, true) {
				t.Fatal("open queue refused data")
			}
			q.close()
			if q.append("must-not-appear", nil, true) {
				t.Fatal("closed queue accepted data")
			}
			var got strings.Builder
			for chunk := q.take(true); chunk.data != nil; chunk = q.take(true) {
				got.Write(chunk.data)
				q.release(chunk.data)
			}
			if !q.drained() || got.String() != "prefix"+message+"\n" {
				t.Fatal("chunk/newline boundary lost bytes")
			}
		})
	}
}

func TestFileBatchesRejectAfterShutdownAndReportConcurrently(t *testing.T) {
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "closed"}, t.TempDir())
	var diagnostics bytes.Buffer
	f.errorWriter = &diagnostics
	stop := startFileLogger(t, f)
	f.Raw("accepted")
	stop()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { f.RawBytes([]byte("rejected")) })
	}
	wg.Wait()
	if n := strings.Count(diagnostics.String(), "logger is shut down"); n != 10 {
		t.Fatalf("rejected calls reported %d errors, want 10", n)
	}
	if readLogFile(t, f.logDir, "closed") != "accepted" {
		t.Fatal("post-shutdown writes changed accepted output")
	}
}

func TestFoutBatchesUseRealFileSink(t *testing.T) {
	for _, tee := range []bool{false, true} {
		t.Run(fmt.Sprint(tee), func(t *testing.T) {
			dir := t.TempDir()
			f := newFile(Strategy{Rotation: SignalRotation, FileBase: "tee"}, dir)
			stdout := &countingWriter{}
			logger := newFoutWithSinks(f, newStdoutWriter(stdout), tee)
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			startLogger(ctx, &wg, logger)
			logger.Log("diagnostic")
			logger.RawBytes([]byte("payload\n"))
			logger.RawWithColors("plain\n", "color\n")
			logger.RawFileOnly("serverless\n")
			logger.LogWithColors("plain diagnostic", "color diagnostic")
			logger.Flush()
			cancel()
			wg.Wait()
			wantFile := "diagnostic\nplain diagnostic\n"
			if tee {
				wantFile = "diagnostic\npayload\nplain\nserverless\nplain diagnostic\n"
			}
			if got := readLogFile(t, dir, "tee"); got != wantFile {
				t.Fatalf("tee %v file bytes = %q, want %q", tee, got, wantFile)
			}
			if got := stdout.String(); got != "diagnostic\npayload\ncolor\ncolor diagnostic\n" {
				t.Fatalf("tee %v changed stdout: %q", tee, got)
			}
		})
	}
}
