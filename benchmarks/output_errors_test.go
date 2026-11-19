package benchmarks

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestResultWritersReportOutputFailure(t *testing.T) {
	logger := NewResultLogger()
	logger.AddResult(BenchmarkResult{Tool: "dcat", Operation: "read"})
	directory := t.TempDir()

	tests := map[string]func() error{
		"JSON":       func() error { return logger.WriteJSON(directory) },
		"CSV":        func() error { return logger.WriteCSV(directory) },
		"Markdown":   func() error { return logger.WriteMarkdown(directory) },
		"comparison": func() error { return WriteComparisonReport(ComparisonReport{}, directory) },
	}

	for name, write := range tests {
		t.Run(name, func(t *testing.T) {
			if err := write(); err == nil {
				t.Fatal("expected output error")
			}
		})
	}
}

func TestGenerateCompressedFileReportsFinalizationFailure(t *testing.T) {
	closeErr := errors.New("finish compression")
	tmpFile := filepath.Join(t.TempDir(), "source.log")
	finalFile := filepath.Join(t.TempDir(), "output.log.compressed")
	config := TestDataConfig{
		Size:        FileSize(1024),
		Format:      SimpleLogFormat,
		Compression: GzipCompression,
	}

	err := generateCompressedFile(tmpFile, finalFile, config, func(w io.Writer) (io.WriteCloser, error) {
		return &closeErrorWriter{Writer: w, closeErr: closeErr}, nil
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("expected compression close error, got %v", err)
	}
	if _, statErr := os.Stat(finalFile); !os.IsNotExist(statErr) {
		t.Fatalf("incomplete output remains after finalization failure: %v", statErr)
	}
}

type closeErrorWriter struct {
	io.Writer
	closeErr error
}

func (w *closeErrorWriter) Close() error {
	return w.closeErr
}
