package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// groupReadFunc reads a file once for the members of a group; see
// readhub.Hub.ReadOnce.
type groupReadFunc func(context.Context, omode.Mode, readhub.Session, readhub.Group, readhub.SlotAcquirer) error

type readShareKeyType struct{}

var readShareKey readShareKeyType

// groupReadFuncFor returns hub's ReadOnce, or nil for no hub.
func groupReadFuncFor(hub *readhub.Hub) groupReadFunc {
	if hub == nil {
		return nil
	}
	return hub.ReadOnce
}

// withReadShareOption keeps the value of a command's config.ReadShareOption
// for the read it starts when the session's user is the scheduler's,
// config.ScheduleUser: only scheduled jobs send the option, and only they may
// start group reads. That user authenticates with a scheduled job's name
// from the job's AllowFrom addresses only (public keys are refused for it),
// so other users cannot start group reads, whose ended groups the hub
// remembers for a while.
func (d *commandDispatcher) withReadShareOption(ctx context.Context, value string) context.Context {
	if value == "" {
		return ctx
	}
	if d.handler == nil || d.handler.user == nil || d.handler.user.Name != config.ScheduleUser {
		if d.handler != nil {
			d.handler.Logger().Debug(d.handler.user, "Ignoring the read share option of a user other than "+
				config.ScheduleUser+", reading privately")
		}
		return ctx
	}
	return contextWithReadShare(ctx, value)
}

// contextWithReadShare keeps value, a command's config.ReadShareOption, in
// ctx for the read it starts, without checking the session's user: only the
// dispatcher's withReadShareOption, and the tests, may call it.
func contextWithReadShare(ctx context.Context, value string) context.Context {
	if value == "" {
		return ctx
	}
	return context.WithValue(ctx, readShareKey, value)
}

// redactReadShare returns command with the value of its read share option,
// which holds the group ID, redacted.
func redactReadShare(command string) string {
	return config.RedactReadShare(command)
}

// argsForLog returns args, the words of a command, with the value of a read
// share option redacted.
func argsForLog(args []string) []string {
	return config.RedactReadShares(args)
}

// commandForLog returns a command as received, "protocol <version> base64
// <command>", for the log. When its base64-encoded command has a read share
// option, it returns it decoded with the option's value redacted instead.
func commandForLog(command string) string {
	words := strings.Split(command, " ")
	if len(words) != 4 || words[0] != "protocol" || words[2] != "base64" {
		return redactReadShare(command)
	}
	decoded, err := base64.StdEncoding.DecodeString(words[3])
	if err != nil {
		return command
	}
	redacted := redactReadShare(string(decoded))
	if redacted == string(decoded) {
		return command
	}
	return strings.Join(words[:3], " ") + " (decoded) " + redacted
}

// readShareGroup returns the group whose one-shot read of a validated file
// the read may join. The group ID only selects a group read; the session
// joins it with its own validated target after its own permission check, and
// the group read takes a cat slot for the session, like a private read.
func (r *readCommand) readShareGroup(ctx context.Context, target *fs.ValidatedReadTarget) (readhub.Group, bool) {
	value, _ := ctx.Value(readShareKey).(string)
	if value == "" || r.readGroup == nil || r.serverless ||
		(r.mode != omode.CatClient && r.mode != omode.GrepClient) ||
		target == nil || target.Kind != fs.FileKind {
		return readhub.Group{}, false
	}
	share, err := config.ParseReadShare(value)
	if err != nil {
		r.logger.Warn(r.logContext, "Ignoring invalid read share option, reading privately", err)
		return readhub.Group{}, false
	}
	return readhub.Group{ID: share.Group, Members: share.Members}, true
}

// groupReadSlots takes a group member's read slots from the session's server,
// as its private read would.
type groupReadSlots struct {
	slots readSlotAcquirer
	mode  omode.Mode
	path  string
}

func (s groupReadSlots) AcquireSlot(ctx context.Context) (func(), bool) {
	return s.slots.AcquireReadSlot(ctx, s.mode, s.path)
}

func (s groupReadSlots) TryAcquireSlot() (func(), bool) {
	return s.slots.TryAcquireReadSlot(s.mode, s.path)
}

// readWithGroup reads the file through its group's one-shot read. The caller
// holds no cat slot: the group read takes the session's slot once the group's
// members are there, so that no member holds a slot while it waits for the
// others. It reports false, having read nothing and holding no slot, when the
// group read had already started or started without a free slot for the
// session; the caller then takes a slot and reads privately.
func (r *readCommand) readWithGroup(ctx context.Context, ltx lcontext.LContext, re regex.Regex,
	options readerFactoryOptions, group readhub.Group) bool {

	path, globID := options.path, options.globID
	r.logger.Info(r.logContext, "Using shared one-shot read", path, globID)
	err := r.readGroup(ctx, r.mode, readhub.Session{
		Target:         *options.target,
		FilePath:       path,
		GlobID:         globID,
		LContext:       ltx,
		Regex:          re,
		ServerMessages: options.serverMessages,
		Logger:         options.logger,
		NewProcessor: func() line.Processor {
			// Called once, and only after the session joined the group.
			return r.makeProcessor(path, globID, r.newLineWriter(ctx, r.generation))
		},
	}, group, groupReadSlots{slots: options.slots, mode: r.mode, path: path})
	switch {
	case errors.Is(err, readhub.ErrGroupReadStarted):
		return false
	case err == nil:
		return true
	}
	// A private one-shot read logs its reader's or processor's error and
	// ends, and a worker panic ends the command as a panic.
	r.logger.Error(r.logContext, path, globID, err)
	if errors.Is(err, fs.ErrReaderWorkerPanic) {
		panic(err)
	}
	r.reportFailure(ctx, readFailureReadingFile)
	return true
}
