package handlers

import (
	"context"
	"time"

	"github.com/mimecast/dtail/internal/ctxutil"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
)

type shutdownCoordinator struct {
	lifecycle  readCommandLifecycle
	aggregates readCommandAggregates
	timings    readTimings
	// aggregate is captured when the read command is admitted. An interactive
	// update may replace the handler's current aggregate before this read exits;
	// completion must still apply only to the generation this read fed.
	aggregate *mapaggregate.Aggregate
	// oneShotInput is true for cat/grep style reads whose input is exhausted
	// once every file has been read to EOF. Follow-mode (tail) reads never
	// exhaust their input, so they must never finish the output aggregate.
	oneShotInput bool
	// inputBatchOwned is set when this read was admitted inside an explicit
	// input batch. The batch's end marker owns FinishInput for this aggregate.
	inputBatchOwned    bool
	unbatchedInputWait func(context.Context, time.Duration) bool
}

type aggregateInputBatchCoordinator interface {
	coordinateAggregateInputCompletion(*mapaggregate.Aggregate) bool
}

// defaultUnbatchedAggregateInputGrace is the grace for one-shot reads that
// arrive outside an input batch (readTimings.unbatchedAggregateInputGrace).
// DTail clients enclose every command stream holding a read in an input
// batch (protocol.InputBatchBeginCommand), and SESSION START and UPDATE
// admit their commands in one, so they never take this path; for a client
// sending reads without the batch markers no protocol event proves that the
// next frame is not another read, so a brief recheck remains the only way to
// cover a following frame that has not reached dispatch yet.
const defaultUnbatchedAggregateInputGrace = 100 * time.Millisecond

func newShutdownCoordinator(lifecycle readCommandLifecycle, aggregates readCommandAggregates,
	timings readTimings, oneShotInput bool, aggregate *mapaggregate.Aggregate) *shutdownCoordinator {
	timings = timings.withDefaults()
	return &shutdownCoordinator{
		lifecycle:    lifecycle,
		aggregates:   aggregates,
		timings:      timings,
		aggregate:    aggregate,
		oneShotInput: oneShotInput,
	}
}

func (c *shutdownCoordinator) onFileProcessed(ctx context.Context, path string) {
	remaining, activeCommands := c.lifecycle.CompletePendingFile()
	c.lifecycle.DebugReadLifecycle("File processing complete", "path", path, "remainingPending", remaining)

	if remaining != 0 {
		return
	}

	// All pending file reads are drained: for one-shot inputs let a output
	// aggregate finish so a blocked server-mode map command can return (see
	// maybeFinishAggregateInput for the circular wait this prevents).
	c.maybeFinishAggregateInput(ctx)

	if activeCommands != 0 {
		return
	}

	c.finalizeWhenIdle(ctx)
}

// maybeFinishAggregateInput signals input-exhausted to the output
// aggregate once all pending one-shot file reads have drained. This is what
// terminates a server-mode output dmap: the map command blocks inside
// Aggregate.Start and keeps the session's active-command count above
// zero, while session shutdown in turn waits for that count to reach zero.
// Without this signal neither side can make progress and the client hangs
// forever even after receiving all results (the aggregate used to be
// finished only at session teardown or generation-replacement Abort).
//
// The aggregate pointer is captured when the read command is admitted and
// compared before finishing, so an interactive :reload cannot have its fresh
// aggregate finished by a stale observation from the old generation.
func (c *shutdownCoordinator) maybeFinishAggregateInput(ctx context.Context) {
	if ctx == nil {
		panic("handlers: nil aggregate completion context")
	}
	if !c.oneShotInput {
		return
	}
	aggregate := c.aggregate
	if aggregate == nil {
		return
	}
	if c.inputBatchOwned {
		return
	}
	if batch, ok := c.aggregates.(aggregateInputBatchCoordinator); ok && batch.coordinateAggregateInputCompletion(aggregate) {
		return
	}

	// Without batch markers there is no protocol event that proves the next
	// transport frame is not another read: recheck once after a bounded
	// grace (defaultUnbatchedAggregateInputGrace).
	wait := c.unbatchedInputWait
	if wait == nil {
		wait = ctxutil.Sleep
	}
	if !wait(ctx, c.timings.unbatchedAggregateInputGrace) {
		return
	}
	if pending, _ := c.lifecycle.PendingAndActive(); pending != 0 {
		return
	}
	if c.aggregates.Aggregate() != aggregate {
		return
	}
	aggregate.FinishInput()
}

func (c *shutdownCoordinator) finalizeWhenIdle(ctx context.Context) {
	// Pending input is registered before work starts. A map command remains
	// active until Aggregate.Start synchronously completes its final
	// Aggregate.Shutdown serialization. Reaching idle is
	// therefore already the aggregate-completion signal; handler shutdown also
	// joins Aggregate.Shutdown defensively.
	finalPending, finalActive := c.lifecycle.PendingAndActive()
	if finalPending == 0 && finalActive == 0 {
		c.lifecycle.DebugReadLifecycle("No active commands and no pending files after double-check, triggering shutdown")
		c.lifecycle.TriggerShutdown(ctx)
		return
	}

	c.lifecycle.DebugReadLifecycle("Shutdown check cancelled", "finalPending", finalPending, "finalActive", finalActive)
}
