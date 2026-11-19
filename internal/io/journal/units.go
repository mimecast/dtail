//go:build linux

package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	// unitFieldName is the journal field whose values ListUnits lists.
	unitFieldName = "_SYSTEMD_UNIT"
	// maxUnitLineBytes is the longest unit name ListUnits keeps; a longer
	// output line is skipped (systemd unit names have at most 255 bytes).
	maxUnitLineBytes = 1024
)

// ListUnits returns the systemd units the journal has entries of
// ("journalctl --field _SYSTEMD_UNIT"), in journalctl's order, at most
// maxUnits of them; truncated reports that journalctl listed more, which
// are then not read. Blank lines and lines longer than maxUnitLineBytes are
// skipped. journalctl is ended when ctx is done. The path listing of the
// probe command (internal/handlers/probe_list.go) gets the units through a
// UnitCache in dserver and filters them by the session user's read
// permissions of journal:<unit>.
func ListUnits(ctx context.Context, maxUnits int) (units []string, truncated bool, err error) {
	path, err := exec.LookPath(journalctlCommand)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrJournalctlNotFound, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--field", unitFieldName)
	cmd.WaitDelay = processTerminateGrace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("journalctl stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start journalctl: %w", err)
	}
	units, truncated, scanErr := scanUnits(stdout, maxUnits)
	if truncated || scanErr != nil {
		// The rest is not read: stop journalctl instead of waiting for it
		// (it could block writing to the pipe nobody reads any more).
		cancel()
	}
	waitErr := cmd.Wait()
	switch {
	case scanErr != nil:
		return nil, false, fmt.Errorf("read journalctl output: %w", scanErr)
	case ctx.Err() != nil && !truncated:
		return nil, false, fmt.Errorf("list journal units: %w", ctx.Err())
	case waitErr != nil && !truncated:
		return nil, false, fmt.Errorf("journalctl --field %s: %w", unitFieldName, waitErr)
	}
	return units, truncated, nil
}

// scanUnits reads at most maxUnits unit names, one per line, from r.
func scanUnits(r io.Reader, maxUnits int) ([]string, bool, error) {
	reader := bufio.NewReaderSize(r, maxUnitLineBytes)
	var units []string
	for {
		line, tooLong, err := readUnitLine(reader)
		if errors.Is(err, io.EOF) {
			return units, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if tooLong || strings.TrimSpace(line) == "" {
			continue
		}
		if len(units) == maxUnits {
			return units, true, nil
		}
		units = append(units, strings.TrimRight(line, "\r"))
	}
}

// readUnitLine reads one line (without its "\n") and reports whether it was
// longer than the reader's buffer; the rest of such a line is discarded.
func readUnitLine(reader *bufio.Reader) (string, bool, error) {
	line, isPrefix, err := reader.ReadLine()
	if err != nil {
		return "", false, err
	}
	if !isPrefix {
		return string(line), false, nil
	}
	for isPrefix {
		if _, isPrefix, err = reader.ReadLine(); err != nil {
			return "", true, err
		}
	}
	return "", true, nil
}
