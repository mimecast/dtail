package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/regex"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// infoRecorder records the hub's INFO lines; it is safe for concurrent use.
type infoRecorder struct {
	logging.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *infoRecorder) Info(args ...any) string {
	message := fmt.Sprint(args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, message)
	return message
}

func (l *infoRecorder) count(substrings ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, line := range l.lines {
		matches := true
		for _, s := range substrings {
			matches = matches && strings.Contains(line, s)
		}
		if matches {
			count++
		}
	}
	return count
}

func TestReadShareGroup(t *testing.T) {
	file := writeSharedReadFile(t, "x\n")
	fileTarget, err := fs.NewValidatedReadTarget(file)
	if err != nil {
		t.Fatal(err)
	}
	journalTarget, err := fs.NewValidatedJournalTarget("journal:dtail.service")
	if err != nil {
		t.Fatal(err)
	}
	readGroup := func(context.Context, omode.Mode, readhub.Session, readhub.Group, readhub.SlotAcquirer) error {
		return nil
	}

	tests := []struct {
		name   string
		option string
		mutate func(*readCommand, **fs.ValidatedReadTarget)
		want   bool
	}{
		{"cat of a file with a hub", "g1:3", nil, true},
		{"grep", "g1:3", func(r *readCommand, _ **fs.ValidatedReadTarget) { r.mode = omode.GrepClient }, true},
		{"no option", "", nil, false},
		{"invalid option", "g1", nil, false},
		{"tail", "g1:3", func(r *readCommand, _ **fs.ValidatedReadTarget) { r.mode = omode.TailClient }, false},
		{"no hub", "g1:3", func(r *readCommand, _ **fs.ValidatedReadTarget) { r.readGroup = nil }, false},
		{"serverless", "g1:3", func(r *readCommand, _ **fs.ValidatedReadTarget) { r.serverless = true }, false},
		{"journal", "g1:3", func(_ *readCommand, target **fs.ValidatedReadTarget) { *target = &journalTarget }, false},
		{"no target", "g1:3", func(_ *readCommand, target **fs.ValidatedReadTarget) { *target = nil }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &readCommand{mode: omode.CatClient, readGroup: readGroup, logger: logging.NopLogger{}}
			target := &fileTarget
			if tt.mutate != nil {
				tt.mutate(r, &target)
			}
			group, ok := r.readShareGroup(contextWithReadShare(context.Background(), tt.option), target)
			if ok != tt.want {
				t.Fatalf("readShareGroup() = %+v, %v, want %v", group, ok, tt.want)
			}
			if ok && (group.ID != "g1" || group.Members != 3) {
				t.Errorf("group = %+v, want g1 with 3 members", group)
			}
		})
	}
}

// groupReadCase is one session of a group, with its own filter.
type groupReadCase struct {
	name  string
	mode  omode.Mode
	ltx   lcontext.LContext
	regex string
}

// drained returns all output the server received once every line written so
// far went through its output collector.
func drained(t *testing.T, server *sharedReadTestServer) string {
	t.Helper()
	const marker = "\x00drained\x00"
	server.outputLines <- []byte(marker)
	waitForShared(t, "the output to drain", func() bool {
		return strings.HasSuffix(server.received(), marker)
	})
	return strings.TrimSuffix(server.received(), marker)
}

func runRead(t *testing.T, server *sharedReadTestServer, rc groupReadCase, path, share string) {
	t.Helper()
	cmd := newReadCommandWithDependencies(server.readCommandDependencies(), rc.mode, nil)
	target, err := server.PrepareReadTarget(path)
	if err != nil {
		t.Fatalf("test setup: no target for %s", path)
	}
	re, err := regex.New(rc.regex, regex.Default)
	if err != nil {
		t.Fatal(err)
	}
	cmd.read(contextWithReadShare(context.Background(), share), rc.ltx, path, &target, "glob", re)
}

func TestGroupReadOutputEqualsPrivateReadOutput(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 2000; i++ {
		switch {
		case i%101 == 0:
			content.WriteString("\n")
		case i%9 == 0:
			fmt.Fprintf(&content, "ERROR line %d\n", i)
		default:
			fmt.Fprintf(&content, "INFO line %d\n", i)
		}
	}
	content.WriteString("ERROR unterminated last line")
	path := writeSharedReadFile(t, content.String())

	cases := []groupReadCase{
		{"cat", omode.CatClient, lcontext.LContext{}, ""},
		{"grep", omode.GrepClient, lcontext.LContext{}, "ERROR"},
		{"grep context", omode.GrepClient, lcontext.LContext{BeforeContext: 2, AfterContext: 1}, "ERROR"},
		{"grep max", omode.GrepClient, lcontext.LContext{MaxCount: 4}, "ERROR"},
		{"grep max context", omode.GrepClient, lcontext.LContext{MaxCount: 2, AfterContext: 3}, "ERROR"},
		{"cat max", omode.CatClient, lcontext.LContext{MaxCount: 7}, ""},
	}
	logger := &infoRecorder{}
	hub := NewReadHub(&config.ServerConfig{MaxConcurrentCats: 8}, logger)
	// Every mode is its own group read; each has as many members as cases.
	membersPerMode := map[omode.Mode]int{}
	for _, rc := range cases {
		membersPerMode[rc.mode]++
	}

	shared := make([]*sharedReadTestServer, len(cases))
	var wg sync.WaitGroup
	for i, rc := range cases {
		shared[i] = newSharedReadTestServer(t, hub)
		share := fmt.Sprintf("group-%s:%d", rc.mode, membersPerMode[rc.mode])
		wg.Add(1)
		go func() {
			defer wg.Done()
			runRead(t, shared[i], rc, path, share)
		}()
	}
	wg.Wait()

	for i, rc := range cases {
		private := newSharedReadTestServer(t, nil)
		runRead(t, private, rc, path, "")
		want := drained(t, private)
		if want == "" {
			t.Fatalf("%s: the private read produced no output", rc.name)
		}
		got := drained(t, shared[i])
		if got != want {
			t.Errorf("%s: group read output (%d bytes) differs from private read output (%d bytes)",
				rc.name, len(got), len(want))
		}
	}
	for mode, members := range membersPerMode {
		group := readhub.Group{ID: "group-" + mode.String()}
		started := logger.count("Shared one-shot read started", group.LogID(),
			fmt.Sprintf("members=%d/%d", members, members))
		if started != 1 {
			t.Errorf("%s: %d group reads started with every member, want 1", mode, started)
		}
	}
}

func TestGroupReadEndings(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantPrivate bool
		wantPanic   bool
	}{
		{"late joiner reads privately", readhub.ErrGroupReadStarted, true, false},
		{"read ended", nil, false, false},
		{"reader error ends the read", errors.New("read failed"), false, false},
		{"reader panic", fmt.Errorf("%w: boom", fs.ErrReaderWorkerPanic), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSharedReadFile(t, "existing\n")
			server := newSharedReadTestServer(t, nil)
			cmd := newReadCommandWithDependencies(server.readCommandDependencies(), omode.CatClient, nil)
			calls := 0
			cmd.readGroup = func(context.Context, omode.Mode, readhub.Session, readhub.Group, readhub.SlotAcquirer) error {
				calls++
				return tt.err
			}
			target, _ := server.PrepareReadTarget(path)

			var recovered any
			func() {
				defer func() { recovered = recover() }()
				cmd.read(contextWithReadShare(context.Background(), "g:2"), lcontext.LContext{}, path,
					&target, "glob", regex.NewNoop())
			}()

			if calls != 1 {
				t.Errorf("the group read was called %d times, want once", calls)
			}
			if (recovered != nil) != tt.wantPanic {
				t.Fatalf("recovered = %v, want panic %v", recovered, tt.wantPanic)
			}
			if tt.wantPrivate {
				waitForShared(t, "the private read", func() bool {
					return strings.Contains(server.received(), "existing")
				})
			} else if strings.Contains(drained(t, server), "existing") {
				t.Error("the file was read privately as well")
			}
		})
	}
}

func TestDispatchCommandKeepsTheReadShareOptionOfTheScheduler(t *testing.T) {
	share := config.ReadShare{Group: "abc", Members: 2}
	args := config.Args{ReadShare: share}
	command := "cat:" + args.SerializeOptions() + " /tmp/file.log noop"

	tests := []struct {
		name string
		user string
		want string
	}{
		{"scheduled job", config.ScheduleUser, share.String()},
		// Other users cannot start group reads, which the hub remembers.
		{"interactive user", "paul", ""},
		{"continuous job", config.ContinuousUser, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionUser, err := user.New(tt.user, "127.0.0.1:1234", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := "unset"
			d := &commandDispatcher{
				handler: &baseHandler{user: sessionUser},
				handleCommandCb: func(ctx context.Context, _ lcontext.LContext, _ int, _ []string, _ string) {
					got, _ = ctx.Value(readShareKey).(string)
				},
			}
			if err := d.handleRawCommand(context.Background(), command); err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("read share option on the command context = %q, want %q", got, tt.want)
			}

			got = "unset"
			if err := d.handleRawCommand(context.Background(), "cat:max=1 /tmp/file.log noop"); err != nil {
				t.Fatal(err)
			}
			if got != "" {
				t.Errorf("a command without the option got %q", got)
			}
		})
	}
}

func TestCommandLogsRedactTheReadShare(t *testing.T) {
	share := config.ReadShare{Group: "secretgroupid", Members: 2}
	args := config.Args{ReadShare: share, LContext: lcontext.LContext{MaxCount: 3}, Quiet: true}
	command := "cat:" + args.SerializeOptions() + " /tmp/file.log noop"
	encodedShare := strings.TrimPrefix(strings.Split(strings.SplitN(command, "share=", 2)[1], " ")[0], "base64%")

	received := "protocol " + protocol.ProtocolCompat + " base64 " + base64.StdEncoding.EncodeToString([]byte(command))
	logged := []string{
		commandForLog(received),
		commandForLog(command),
		redactReadShare(command),
		strings.Join(argsForLog(strings.Split(command, " ")), " "),
	}
	for _, line := range logged {
		if strings.Contains(line, share.Group) || strings.Contains(line, encodedShare) ||
			strings.Contains(line, base64.StdEncoding.EncodeToString([]byte(command))) {
			t.Errorf("log line %q has the read share", line)
		}
		for _, want := range []string{"max=3", "quiet=true", "share=REDACTED", "/tmp/file.log"} {
			if !strings.Contains(line, want) {
				t.Errorf("log line %q lacks %q", line, want)
			}
		}
	}

	// Commands without the option are logged as received.
	plain := "protocol " + protocol.ProtocolCompat + " base64 " + base64.StdEncoding.EncodeToString([]byte("cat:max=3 /tmp/f noop"))
	if got := commandForLog(plain); got != plain {
		t.Errorf("commandForLog(%q) = %q, want it unchanged", plain, got)
	}
}

// Two sessions read the same files through group reads, each file in its
// own goroutine like a glob read, sharing the server's cat slots. A member
// must not hold its slot while it waits for the other session's member:
// with fewer slots than files, the members holding the slots would wait for
// members that wait for a slot, until the group wait of 3 s ran out, and
// most files would be read once per session.
func TestGroupMembersWaitWithoutACatSlot(t *testing.T) {
	const files = 6
	tests := []struct {
		name     string
		cats     int
		started  string
		privates int
	}{
		// Each group read takes the slots free when it starts, without
		// waiting for more; its members without one read privately (-1: as
		// many as the slots left over).
		{"two slots", 2, "members=", -1},
		// Fewer slots than the group has members, as on a dserver whose
		// MaxConcurrentCats is lower than the scheduling dserver's: each
		// group read admits one member, the other reads privately at once.
		{"one slot", 1, "members=1/1", files},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			for i := range files {
				paths = append(paths, writeSharedReadFile(t, fmt.Sprintf("file %d line\n", i)))
			}
			logger := &infoRecorder{}
			hub := NewReadHub(&config.ServerConfig{MaxConcurrentCats: tt.cats}, logger)
			catLimiter := make(chan struct{}, tt.cats)
			sessions := []*sharedReadTestServer{newSharedReadTestServer(t, hub), newSharedReadTestServer(t, hub)}

			began := time.Now()
			var wg sync.WaitGroup
			for s, session := range sessions {
				session.catLimiter = catLimiter
				for i := range files {
					// The sessions start their reads in opposite orders.
					path := paths[i]
					if s == 1 {
						path = paths[files-1-i]
					}
					wg.Go(func() {
						runRead(t, session, groupReadCase{"cat", omode.CatClient, lcontext.LContext{}, ""},
							path, "raw-group-id:2")
					})
				}
			}
			finished := make(chan struct{})
			go func() {
				wg.Wait()
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(sharedReadTimeout):
				t.Fatal("the reads did not finish: group reads wait for cat slots that their members hold")
			}
			elapsed := time.Since(began)

			if elapsed >= readhub.DefaultGroupWait {
				t.Errorf("the reads took %v, a group waited for a member", elapsed)
			}
			group := readhub.Group{ID: "raw-group-id"}
			if got := logger.count("Shared one-shot read started", group.LogID(), tt.started); got != files {
				t.Errorf("%d group reads started with %s, want %d", got, tt.started, files)
			}
			shared := logger.count("Shared one-shot read started", group.LogID(), "members=2/2")*2 +
				logger.count("Shared one-shot read started", group.LogID(), "members=1/")
			privates := tt.privates
			if privates < 0 {
				privates = 2*files - shared
			}
			if got := logger.count("reading privately", group.LogID()); got != privates {
				t.Errorf("%d members read privately, want %d", got, privates)
			}
			if got := logger.count(group.ID); got != 0 {
				t.Errorf("%d log lines contain the raw group ID", got)
			}
			for _, session := range sessions {
				output := drained(t, session)
				for i := range files {
					if want := fmt.Sprintf("file %d line", i); strings.Count(output, want) != 1 {
						t.Errorf("session output has %q %d times, want once", want, strings.Count(output, want))
					}
				}
			}
			if len(catLimiter) != 0 {
				t.Errorf("%d cat slots still in use", len(catLimiter))
			}
		})
	}
}
