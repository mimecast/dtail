package loggers

import (
	"bytes"
	"os"
	"testing"
)

func TestFoutFileByteTeeOwnsBytesAndNeverWritesStdout(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := newFile(Strategy{Rotation: SignalRotation, FileBase: "tee"}, dir)
			stdout := &recordingSink{}
			f := newFoutWithSinks(file, stdout, enabled)
			// Enqueue before starting so only an owned copy survives mutation.
			data := []byte("borrowed\x00plain\n")
			f.RawFileOnlyBytes(data)
			clear(data)
			stop := startFileLogger(t, file)
			large := bytes.Repeat([]byte("long"), fileWriterBufSize)
			f.RawFileOnlyBytes(large)
			clear(large)
			f.RawFileOnlyBytes(nil)
			f.Flush()
			stop()
			if stdout.rawCount()+stdout.logCount() != 0 {
				t.Fatal("file-only tee wrote stdout")
			}
			if enabled {
				want := "borrowed\x00plain\n" + string(bytes.Repeat([]byte("long"), fileWriterBufSize))
				if got := readLogFile(t, dir, "tee"); got != want {
					t.Fatal("file tee lost or aliased bytes")
				}
			} else {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("disabled tee created files: %v, %v", entries, err)
				}
			}
		})
	}
}

func TestFoutFileByteTeeFallsBackToStringSink(t *testing.T) {
	file, stdout := &recordingSink{}, &recordingSink{}
	f := newFoutWithSinks(file, stdout, true)
	data := []byte("payload\n")
	f.RawFileOnlyBytes(data)
	clear(data)
	if len(file.raws) != 1 || file.raws[0] != "payload\n" || stdout.rawCount() != 0 {
		t.Fatalf("file=%q stdout=%q", file.raws, stdout.raws)
	}
}
