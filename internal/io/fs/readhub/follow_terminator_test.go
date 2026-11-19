package readhub

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func TestSharedFollowPreservesLegacyCRPayload(t *testing.T) {
	for _, before := range []int{0, 1} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			file := newTestFile(t)
			hub := newTestHub()
			follower := startFollower(t, hub, file, lcontext.LContext{BeforeContext: before}, regex.NewNoop(), "source")
			waitFor(t, "subscriber", func() bool { return subscriberCount(hub, file.path) == 1 })
			file.appendRaw("first\r\nsecond\r\n")
			waitFor(t, "CRLF lines", func() bool { return len(follower.recorder.lines()) == 2 })
			if got, want := follower.recorder.lines(), []string{"first\r", "second\r"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("lines=%q, want=%q", got, want)
			}
		})
	}
}

func TestSharedSnapshotPreservesPhysicalTerminators(t *testing.T) {
	path := writeFile(t, "LF\nCRLF\r\n\nunterminated\r")
	hub := newGroupHub(time.Second)
	member := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), Group{ID: "terminators", Members: 1})
	if err := member.wait(t); err != nil {
		t.Fatal(err)
	}
	if got, want := member.recorder.lines(), []string{"LF\n", "CRLF\r\n", "\n", "unterminated\r"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot lines=%q, want=%q", got, want)
	}
}
