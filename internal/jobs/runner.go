// Package jobs runs dserver's scheduled and continuous MapReduce client workloads.
package jobs

import (
	"context"
	"sync"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
)

// Runner owns the background workloads configured for one dserver process.
type Runner struct {
	scheduler  *scheduler
	continuous *continuous
}

// New returns a background job runner.
func New(cfg config.RuntimeConfig, loggers clients.LoggerDependencies, colorizers ...*brush.Brush) *Runner {
	colorizer := firstColorizer(colorizers)
	return &Runner{
		scheduler:  newScheduler(cfg, loggers, colorizer),
		continuous: newContinuous(cfg, loggers, colorizer),
	}
}

func firstColorizer(colorizers []*brush.Brush) *brush.Brush {
	if len(colorizers) == 0 {
		return nil
	}
	return colorizers[0]
}

// Start runs scheduled and continuous workloads until the context is canceled.
func (r *Runner) Start(ctx context.Context) {
	if r == nil || r.scheduler == nil || r.continuous == nil {
		return
	}

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		r.scheduler.start(ctx)
	}()
	go func() {
		defer workers.Done()
		r.continuous.start(ctx)
	}()
	workers.Wait()
}
