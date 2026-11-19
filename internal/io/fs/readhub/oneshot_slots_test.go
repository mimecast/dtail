package readhub

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// slots is a cat slot limiter like dserver's, counting the slots in use.
type slots struct {
	limiter chan struct{}
	mu      sync.Mutex
	maxUsed int
}

func newSlots(capacity int) *slots {
	return &slots{limiter: make(chan struct{}, capacity)}
}

func (s *slots) AcquireSlot(ctx context.Context) (func(), bool) {
	select {
	case s.limiter <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	return s.taken()
}

func (s *slots) TryAcquireSlot() (func(), bool) {
	select {
	case s.limiter <- struct{}{}:
	default:
		return nil, false
	}
	return s.taken()
}

func (s *slots) taken() (func(), bool) {
	s.mu.Lock()
	s.maxUsed = max(s.maxUsed, len(s.limiter))
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { <-s.limiter }) }, true
}

func (s *slots) inUse() int { return len(s.limiter) }

func TestReadOnceMembersHoldNoSlotWhileTheyWait(t *testing.T) {
	path := writeFile(t, "a\nb\n")
	hub := newGroupHub(time.Hour)
	seams := &countingSeams{release: make(chan struct{})}
	seams.install(hub)
	catSlots := newSlots(2)
	group := Group{ID: "g", Members: 2}

	first := startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(),
		group, catSlots)
	waitFor(t, "the first member to join", func() bool { return groupMembers(hub, path) == 1 })
	time.Sleep(20 * time.Millisecond)
	if used := catSlots.inUse(); used != 0 {
		t.Fatalf("a waiting member holds %d cat slots, want none", used)
	}

	second := startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(),
		group, catSlots)
	waitFor(t, "the group read to start", func() bool { return seams.starts() == 1 })
	if used := catSlots.inUse(); used != 2 {
		t.Errorf("the group read holds %d cat slots, want one per member", used)
	}
	close(seams.release)
	for _, m := range []*testMember{first, second} {
		if err := m.wait(t); err != nil {
			t.Fatalf("ReadOnce() = %v", err)
		}
		if got := m.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n", "b\n"}) {
			t.Errorf("lines = %q", got)
		}
	}
	if used := catSlots.inUse(); used != 0 {
		t.Errorf("%d cat slots still in use after the read, want none", used)
	}
}

// Two sessions each read the same files, joining the group reads in opposite
// orders, with fewer cat slots than files times sessions. A member that held
// a slot while it waited for the other session would keep that session's
// member from joining; the group reads would only start after the group wait
// (an hour here) with one member each. A member without a free slot when its
// group read starts reads privately (ErrGroupReadStarted, nothing fed).
func TestReadOnceGroupReadsNeverWaitForEachOthersSlots(t *testing.T) {
	const files = 6
	hub := newGroupHub(time.Hour)
	catSlots := newSlots(2)
	var paths []string
	for range files {
		paths = append(paths, writeFile(t, "a\nb\nc\n"))
	}
	group := Group{ID: "g", Members: 2}
	var members []*testMember
	for i := range files {
		members = append(members,
			startSlottedMember(t, hub, omode.CatClient, paths[i], lcontext.LContext{}, regex.NewNoop(),
				group, catSlots),
			startSlottedMember(t, hub, omode.CatClient, paths[files-1-i], lcontext.LContext{}, regex.NewNoop(),
				group, catSlots))
	}
	for _, m := range members {
		err := m.wait(t)
		if errors.Is(err, ErrGroupReadStarted) {
			if events := m.recorder.snapshot(); len(events) != 0 {
				t.Errorf("a member reading privately got %v", events)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ReadOnce() = %v", err)
		}
		if got := m.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n", "b\n", "c\n"}) {
			t.Errorf("lines = %q", got)
		}
	}
	if catSlots.maxUsed > 2 {
		t.Errorf("up to %d cat slots were in use, want at most 2", catSlots.maxUsed)
	}
	if used := catSlots.inUse(); used != 0 {
		t.Errorf("%d cat slots still in use, want none", used)
	}
}

func TestReadOnceAdmitsAtMostMaxGroupMembers(t *testing.T) {
	path := writeFile(t, "a\n")
	logger := &lineLogger{}
	hub := New(Options{Logger: logger, GroupWait: time.Hour, MaxGroupMembers: 2})
	seams := &countingSeams{release: make(chan struct{})}
	seams.install(hub)
	group := Group{ID: "secret-group-id", Members: 3}

	// Two of three members fill the group: its read starts without waiting
	// an hour for the third.
	members := []*testMember{
		startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
		startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group),
	}
	waitFor(t, "the group read to start", func() bool { return seams.starts() == 1 })
	third := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), (&recorder{}).newProcessor, nil)
	if err := hub.ReadOnce(context.Background(), omode.CatClient, third, group, freeSlot); !errors.Is(err, ErrGroupReadStarted) {
		t.Errorf("third ReadOnce() = %v, want ErrGroupReadStarted", err)
	}
	close(seams.release)
	for _, m := range members {
		if err := m.wait(t); err != nil {
			t.Fatalf("ReadOnce() = %v", err)
		}
	}
	if logger.count("Shared one-shot read started", group.LogID(), "members=2/2") != 1 {
		t.Errorf("log = %q, want one read started with members=2/2", logger.all())
	}
	if strings.Contains(strings.Join(logger.all(), "\n"), group.ID) {
		t.Errorf("the log contains the group ID: %q", logger.all())
	}
}

func TestGroupEntryRefusesMembersBeyondMaxGroupMembers(t *testing.T) {
	path := writeFile(t, "a\n")
	session := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), (&recorder{}).newProcessor, nil)
	e := newGroupEntry(groupKey{path: path}, Group{ID: "g", Members: 3}, path,
		Options{MaxGroupMembers: 2}, logging.NopLogger{}, defaultSeams())
	for i, want := range []bool{true, true, false} {
		member := &groupMember{subscriber: newSubscriber(session, 1), acquire: freeSlot}
		if got := e.add(member); got != want {
			t.Errorf("member %d joined = %v, want %v", i+1, got, want)
		}
	}
}

func TestReadOnceRemembersGroupReadsThatEnded(t *testing.T) {
	path := writeFile(t, "a\n")
	const memory = 300 * time.Millisecond
	hub := New(Options{GroupWait: 10 * time.Millisecond, GroupMemory: memory})
	group := Group{ID: "g", Members: 2}

	// The only member reads alone after the group wait.
	alone := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group)
	if err := alone.wait(t); err != nil {
		t.Fatalf("ReadOnce() = %v", err)
	}
	ended := time.Now()

	// The other member arrives after the read ended: it reads privately at
	// once instead of waiting for a group read of its own.
	late := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), (&recorder{}).newProcessor, nil)
	if err := hub.ReadOnce(context.Background(), omode.CatClient, late, group, freeSlot); !errors.Is(err, ErrGroupReadStarted) {
		t.Fatalf("late ReadOnce() = %v, want ErrGroupReadStarted", err)
	}
	if time.Since(ended) >= memory {
		t.Skip("the late member came too late for this test to tell anything")
	}

	// Once the hub forgot the read, the group ID starts a new group read.
	time.Sleep(memory)
	again := startGroupMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group)
	if err := again.wait(t); err != nil {
		t.Fatalf("ReadOnce() after the group memory = %v", err)
	}
	if got := again.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n"}) {
		t.Errorf("lines = %q", got)
	}
	hub.mu.Lock()
	remembered := len(hub.oneshot.ended)
	hub.mu.Unlock()
	if remembered != 1 {
		t.Errorf("the hub remembers %d ended group reads, want only the last one", remembered)
	}
}

func TestReadOnceMemberLeavingWhileWaitingForItsSlot(t *testing.T) {
	path := writeFile(t, "a\n")
	hub := newGroupHub(time.Hour)
	catSlots := newSlots(1)
	group := Group{ID: "g", Members: 2}

	// Another read holds the only slot.
	release, _ := catSlots.AcquireSlot(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	leaving := &recorder{}
	leavingDone := make(chan error, 1)
	session := groupSession(t, path, lcontext.LContext{}, regex.NewNoop(), leaving.newProcessor, nil)
	go func() { leavingDone <- hub.ReadOnce(ctx, omode.CatClient, session, group, catSlots) }()
	staying := startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(),
		group, catSlots)
	waitFor(t, "both members to join", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		for _, e := range hub.oneshot.entries {
			e.mu.Lock()
			started := e.started
			e.mu.Unlock()
			return started
		}
		return false
	})

	cancel()
	if err := <-leavingDone; err != nil {
		t.Errorf("leaving ReadOnce() = %v, want nil", err)
	}
	release()
	if err := staying.wait(t); err != nil {
		t.Fatalf("ReadOnce() = %v", err)
	}
	if got := staying.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n"}) {
		t.Errorf("lines = %q", got)
	}
	if len(leaving.lines()) != 0 {
		t.Error("the member that left got lines")
	}
	if used := catSlots.inUse(); used != 0 {
		t.Errorf("%d cat slots still in use, want none", used)
	}
}

// lineLogger records every log line.
type lineLogger struct {
	textLogger
	mu    sync.Mutex
	lines []string
}

func (l *lineLogger) Info(args ...any) string {
	message := fmt.Sprint(args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, message)
	return message
}

func (l *lineLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *lineLogger) count(substrings ...string) int {
	count := 0
	for _, line := range l.all() {
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

// A long private cat holds one of two cat slots when a group of two arrives.
// The group read takes the free slot and reads with one member at once; the
// other member reads privately. The group never holds a slot while it waits
// for another, so a cat arriving after the group gets the slot as soon as
// the short group read ended, not after the long cat.
func TestReadOnceGroupHoldsNoSlotWhileWaitingForAnother(t *testing.T) {
	catSlots := newSlots(2)
	releaseLong, _ := catSlots.AcquireSlot(context.Background())
	defer releaseLong()

	path := writeFile(t, "a\nb\n")
	logger := &lineLogger{}
	hub := New(Options{Logger: logger, GroupWait: time.Hour, MaxGroupMembers: 2})
	group := Group{ID: "g", Members: 2}
	members := []*testMember{
		startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group, catSlots),
		startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group, catSlots),
	}
	var read, private int
	for _, m := range members {
		switch err := m.wait(t); {
		case errors.Is(err, ErrGroupReadStarted):
			private++
			if events := m.recorder.snapshot(); len(events) != 0 {
				t.Errorf("the member reading privately got %v", events)
			}
		case err != nil:
			t.Fatalf("ReadOnce() = %v", err)
		default:
			read++
			if got := m.recorder.lines(); !reflect.DeepEqual(got, []string{"a\n", "b\n"}) {
				t.Errorf("lines = %q", got)
			}
		}
	}
	if read != 1 || private != 1 {
		t.Errorf("%d members read in the group and %d privately, want 1 and 1", read, private)
	}
	if logger.count("Shared one-shot read started", "members=1/2") != 1 {
		t.Errorf("log = %q, want one group read started with members=1/2", logger.all())
	}
	if logger.count("no free cat slot", group.LogID()) != 1 {
		t.Errorf("log = %q, want one member reading privately for want of a slot", logger.all())
	}

	// The long cat still holds its slot; the other one is free again.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, acquired := catSlots.AcquireSlot(ctx)
	if !acquired {
		t.Fatal("a cat after the group read got no slot while the long cat ran: the group held it")
	}
	release()
}

// All cat slots are busy when the group read starts: it waits for one slot,
// holding none, then reads with the one member; the other member, whose slot
// is not free then, reads privately.
func TestReadOnceGroupWaitsForOneSlotHoldingNone(t *testing.T) {
	catSlots := newSlots(1)
	releaseOther, _ := catSlots.AcquireSlot(context.Background())

	path := writeFile(t, "a\n")
	hub := New(Options{Logger: logging.NopLogger{}, GroupWait: time.Hour, MaxGroupMembers: 2})
	seams := &countingSeams{release: make(chan struct{})}
	seams.install(hub)
	group := Group{ID: "g", Members: 2}
	members := []*testMember{
		startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group, catSlots),
		startSlottedMember(t, hub, omode.CatClient, path, lcontext.LContext{}, regex.NewNoop(), group, catSlots),
	}
	waitFor(t, "both members to join", func() bool { return groupMembers(hub, path) == 2 })
	time.Sleep(20 * time.Millisecond)
	if used := catSlots.inUse(); used != 1 || seams.starts() != 0 {
		t.Fatalf("with no free slot: %d slots in use, %d reads started, want 1 and 0", used, seams.starts())
	}
	releaseOther()
	waitFor(t, "the group read to start", func() bool { return seams.starts() == 1 })
	close(seams.release)

	var read, private int
	for _, m := range members {
		switch err := m.wait(t); {
		case errors.Is(err, ErrGroupReadStarted):
			private++
		case err != nil:
			t.Fatalf("ReadOnce() = %v", err)
		default:
			read++
		}
	}
	if read != 1 || private != 1 {
		t.Errorf("%d members read in the group and %d privately, want 1 and 1", read, private)
	}
	if used := catSlots.inUse(); used != 0 {
		t.Errorf("%d cat slots still in use, want none", used)
	}
}

func TestReadOnceForgetsTheOldestEndedGroupReads(t *testing.T) {
	const memory = time.Minute
	hub := New(Options{Logger: logging.NopLogger{}, GroupMemory: memory, MaxEndedGroups: 3})
	hub.oneshot.ended = make(map[groupKey]time.Time)
	key := func(i int) groupKey { return groupKey{path: "/f", group: fmt.Sprintf("g%d", i)} }
	start := time.Now()
	at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Second) }

	for i := range 5 {
		hub.rememberEnded(key(i), at(i))
	}
	// The cap keeps the three that ended last.
	for i, want := range []bool{false, false, true, true, true} {
		if got := hub.endedRecently(key(i), at(5)); got != want {
			t.Errorf("group %d remembered = %v, want %v", i, got, want)
		}
	}
	if len(hub.oneshot.ended) != 3 || len(hub.oneshot.endedOrder) != 3 {
		t.Fatalf("remembered %d/%d ended group reads, want 3", len(hub.oneshot.ended), len(hub.oneshot.endedOrder))
	}
	// The group memory forgets them in the order they ended.
	if hub.endedRecently(key(2), at(2).Add(memory)) {
		t.Error("group 2 remembered after the group memory")
	}
	if !hub.endedRecently(key(3), at(2).Add(memory)) {
		t.Error("group 3 forgotten before the group memory passed")
	}
	if len(hub.oneshot.ended) != 2 {
		t.Errorf("remembered %d ended group reads, want 2", len(hub.oneshot.ended))
	}

	// A group read of a key that ended again is remembered from then on.
	hub.rememberEnded(key(3), at(10))
	if !hub.endedRecently(key(3), at(3).Add(memory)) {
		t.Error("group 3 forgotten with the time it ended first")
	}
}

// Looking up an ended group read looks at the oldest remembered ones only,
// not at every one: a join costs the same with many ended group reads.
func TestReadOnceEndedLookupLooksAtTheOldestOnly(t *testing.T) {
	hub := New(Options{Logger: logging.NopLogger{}, MaxEndedGroups: 100000})
	hub.oneshot.ended = make(map[groupKey]time.Time)
	now := time.Now()
	for i := range 100000 {
		hub.rememberEnded(groupKey{group: fmt.Sprint(i)}, now)
	}
	looked := 0
	hub.oneshot.forgetEnded(func(endedGroup, int) bool {
		looked++
		return false
	})
	if looked != 1 {
		t.Errorf("looked at %d ended group reads, want 1", looked)
	}
	if !hub.endedRecently(groupKey{group: "99999"}, now) {
		t.Error("the last ended group read is not remembered")
	}
}
