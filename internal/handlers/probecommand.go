package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/journal"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/mapr/probe"
	"github.com/mimecast/dtail/internal/protocol"
)

// probeInFlightWait bounds how long the command input waits until the
// previous probe's answer is queued (startProbe), i.e. for the session's
// probeInFlight token. It is well under the client's 2 s reload
// acknowledgement timeout, so a SESSION UPDATE that arrives behind two
// probes is still applied in time.
const probeInFlightWait = 500 * time.Millisecond

// handleProbeCommand handles "probe <base64 json>" (protocol.ProbeRequest):
// it samples the requested files, or lists the paths beneath a prefix
// (probe_list.go), and answers, once, with the hidden control frame
// ".syn probe <id> <base64 json>" (protocol.ProbeAnswer). See
// docs/dev/probe.md.
//
// A probe is session-side work next to the session's workload:
//   - It is sent without a session generation, so it neither replaces the
//     running workload nor is its answer dropped as stale output.
//   - It takes no cat or tail limiter slot and adds no pending file, and
//     its sampling is bounded (protocol.ProbeMaxFiles files of a bounded
//     sample each, probeTimeout).
//   - Every failure is an entry of the answer's Errors, never a failed
//     command (protocol.HiddenCommandFailedPrefix), so a probe changes no
//     exit status.
//   - It never ends the session (shutdownOnCompletion): its completion
//     only starts the idle shutdown other work of the session already asked
//     for while it ran.
//   - One probe runs at a time per session; another one meanwhile is
//     answered with protocol.ProbeErrorBusy at once.
//   - Unsent answers are bounded: see startProbe.
//
// The invalid and busy answers are queued from the caller, the session's
// command input, without waiting (trySendProbeAnswer): when the session's
// server messages are full they are dropped, as the command input must not
// stop for them (a SESSION UPDATE behind them would wait too). The client's
// wait for such an answer times out then.
func (h *ServerHandler) handleProbeCommand(ctx context.Context, _ lcontext.LContext,
	argc int, args []string, commandFinished func()) {

	request, ok := h.decodeProbeRequest(argc, args)
	if !ok {
		defer commandFinished()
		h.trySendProbeAnswer(probeErrorAnswer(protocol.ProbeUnknownID, protocol.ProbeErrorInvalidRequest))
		return
	}
	if !h.probeRunning.CompareAndSwap(false, true) {
		defer commandFinished()
		h.trySendProbeAnswer(probeErrorAnswer(request.ID, protocol.ProbeErrorBusy))
		return
	}
	h.startProbe(ctx, request, commandFinished)
}

// startProbe runs the probe of request, whose caller took the probe slot
// (probeRunning), on a goroutine of its own and answers it there.
//
// The slot is given back before the answer is queued, so that a client
// sending the next probe as soon as the answer arrived is not told it is
// busy. A probe whose answer is not queued yet must still not let the next
// probe run, or a client that keeps sending probes without reading makes
// dserver sample every one of them and keep a goroutine and an answer per
// probe. So each probe also holds the session's probeInFlight token from its
// start until its answer was queued, and the next probe needs the token
// before it runs (takeProbeInFlight): at most one answer per session waits.
//
// The command input waits for the token at most probeInFlightWait (500 ms).
// An answer can stay unqueued for long also while the client reads: the
// session output reads payload before server messages, so a sustained
// follow flood leaves them unread. Waiting longer would stop the command
// input, and a SESSION UPDATE behind the probe would not be applied in time
// for the client's reload. A probe that gets no token in time is answered
// busy instead, without waiting (trySendProbeAnswer). Should the session end
// while waiting, the probe neither runs nor answers.
func (h *ServerHandler) startProbe(ctx context.Context, request protocol.ProbeRequest,
	commandFinished func()) {

	if !h.takeProbeInFlight(ctx) {
		defer commandFinished()
		h.probeRunning.Store(false)
		if ctx.Err() == nil {
			h.trySendProbeAnswer(probeErrorAnswer(request.ID, protocol.ProbeErrorBusy))
		}
		return
	}
	go func() {
		// release gives the probe slot back exactly once: early, before the
		// answer is sent, or on a panic. A second clear after the send would
		// free the slot of a probe that took it while this one's send was
		// blocked (a slow client).
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { h.probeRunning.Store(false) }) }
		defer recoverHandlerPanic(h.Logger(), h.user, "probe command cleanup", h.abortAfterPanic)
		defer commandFinished()
		defer func() { <-h.probeInFlight }()
		defer release()
		defer recoverHandlerPanic(h.Logger(), h.user, "probe command", h.abortAfterPanic)
		answer := h.newProber().run(ctx, request)
		release()
		h.sendProbeAnswer(ctx, answer)
	}()
}

// takeProbeInFlight takes the session's probeInFlight token. It waits at
// most probeInFlightWait (a test may set h.probeInFlightWait) and reports
// false when that ran out or ctx ended first.
func (h *ServerHandler) takeProbeInFlight(ctx context.Context) bool {
	wait := h.probeInFlightWait
	if wait <= 0 {
		wait = probeInFlightWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case h.probeInFlight <- struct{}{}:
		return true
	case <-timer.C:
		h.Logger().Info(h.user, "Probe: the previous answer is not queued yet, answering busy")
		return false
	case <-ctx.Done():
		return false
	}
}

// decodeProbeRequest decodes the argument of a probe command.
func (h *ServerHandler) decodeProbeRequest(argc int, args []string) (protocol.ProbeRequest, bool) {
	if argc != 2 {
		h.Logger().Warn(h.user, "Probe: invalid command", "argc", argc)
		return protocol.ProbeRequest{}, false
	}
	request, err := protocol.DecodeProbeRequest(args[1])
	if err != nil {
		h.Logger().Warn(h.user, "Probe: invalid request", err)
		return protocol.ProbeRequest{}, false
	}
	return request, true
}

// newProber returns the prober of this session: the session user's read
// permission check (the quiet one, probeReadTarget), the server's limits
// and default log format.
func (h *ServerHandler) newProber() *prober {
	sample := h.probeSampler
	if sample == nil {
		sample = probe.Sample
	}
	listUnits := h.probeUnitLister
	if listUnits == nil {
		listUnits = journal.ListUnits
	}
	return &prober{
		resolve: h.probeReadTarget,
		pathRules: func() listPathRules {
			return h.user.PathRules(readFilesPermission)
		},
		sample:           sample,
		listUnits:        listUnits,
		listLimits:       h.probeListLimits.withDefaults(),
		listDisabled:     h.serverCfg.PathCompletionDisable,
		journalSupported: h.journalSupported,
		maxGlobTargets:   h.readTimings.maxGlobTargets,
		maxLineLength:    h.readTimings.maxLineLength,
		hostname:         h.hostname,
		defaultLogFormat: h.serverCfg.MapreduceLogFormat,
		logger:           h.Logger(),
		logContext:       h.user,
	}
}

// sendProbeAnswer queues the answer frame, cut to
// protocol.ProbeMaxAnswerBytes (marshalProbeAnswer). It sends nothing once
// ctx is done (the session ends), so a probe never holds up a shutdown.
func (h *ServerHandler) sendProbeAnswer(ctx context.Context, answer protocol.ProbeAnswer) {
	if message, ok := h.probeAnswerMessage(answer); ok {
		// Logged before the send: the reader may take the answer before
		// SendReadMessage returns, and the send only gives up when the
		// session ends.
		h.logProbeAnswered(answer, len(message))
		h.SendReadMessage(ctx, 0, message)
	}
}

// trySendProbeAnswer queues the answer frame like sendProbeAnswer if the
// session's server messages have room, and drops it otherwise. The command
// input sends the busy and invalid answers with it, so it never waits for a
// client that does not read them (handleProbeCommand).
func (h *ServerHandler) trySendProbeAnswer(answer protocol.ProbeAnswer) {
	message, ok := h.probeAnswerMessage(answer)
	if !ok {
		return
	}
	select {
	case h.serverMessages <- encodeGeneratedMessage(0, message+"\n"):
		h.logProbeAnswered(answer, len(message))
	default:
		h.Logger().Info(h.user, "Probe: server messages full, dropping answer", "id", answer.ID)
	}
}

// probeAnswerMessage encodes the hidden answer message of answer.
func (h *ServerHandler) probeAnswerMessage(answer protocol.ProbeAnswer) (string, bool) {
	data, err := marshalProbeAnswer(answer, protocol.ProbeMaxAnswerBytes)
	if err != nil {
		// Plain maps, strings and numbers always encode; this is a guard.
		h.Logger().Error(h.user, "Probe: unable to encode answer", err)
		return "", false
	}
	return protocol.EncodeProbeAnswer(answer.ID, data), true
}

// logProbeAnswered logs the summary line of an answer (bytes: the size of
// its encoded message) that is sent; a busy or invalid answer dropped as the
// server messages are full logs its own line instead (trySendProbeAnswer).
// The table of a table-targeted probe is logged too (cut like an error's
// entry; a valid table is at most protocol.ProbeMaxTableBytes anyway).
func (h *ServerHandler) logProbeAnswered(answer protocol.ProbeAnswer, bytes int) {
	args := []any{"Probe answered", "id", answer.ID, "files", answer.FilesSampled,
		"lines", answer.LinesSampled, "formats", len(answer.Formats), "entries", len(answer.Entries),
		"errors", len(answer.Errors), "bytes", bytes}
	if answer.Table != "" {
		args = append(args, "table", cappedProbeEntry(answer.Table))
	}
	h.Logger().Info(append([]any{h.user}, args...)...)
}

// probeErrorAnswer is the answer of a probe that failed with reason.
func probeErrorAnswer(id, reason string) protocol.ProbeAnswer {
	return protocol.ProbeAnswer{ID: id, Errors: []protocol.ProbeError{{Error: reason}}}
}
