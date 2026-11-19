package mapr

import (
	"errors"
	"reflect"
	"testing"
)

type recordingCloser struct {
	events   *[]string
	closeErr error
}

func (c recordingCloser) Close() error {
	*c.events = append(*c.events, "close")
	return c.closeErr
}

func TestCloseAndRenameWrittenFileClosesBeforeRename(t *testing.T) {
	var events []string
	err := closeAndRenameWrittenFile(
		recordingCloser{events: &events},
		nil,
		"result.tmp",
		"result.csv",
		func(_, _ string) error {
			events = append(events, "rename")
			return nil
		},
		func(string) error {
			events = append(events, "remove")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("closeAndRenameWrittenFile: %v", err)
	}
	if want := []string{"close", "rename"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestCloseAndRenameWrittenFileDoesNotPublishAfterCloseFailure(t *testing.T) {
	closeErr := errors.New("disk flush failed")
	var events []string
	err := closeAndRenameWrittenFile(
		recordingCloser{events: &events, closeErr: closeErr},
		nil,
		"result.tmp",
		"result.csv",
		func(_, _ string) error {
			events = append(events, "rename")
			return nil
		},
		func(string) error {
			events = append(events, "remove")
			return nil
		},
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want close error", err)
	}
	if want := []string{"close", "remove"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
