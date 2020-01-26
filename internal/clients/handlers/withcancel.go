package handlers

import "context"

type withCancel struct {
	ctx  context.Context
	done chan struct{}
}

// WithCancel sets and returns the context used.
func (w *withCancel) WithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	cancelCtx, cancel := context.WithCancel(ctx)
	w.ctx = cancelCtx

	return cancelCtx, cancel
}

func (w *withCancel) Done() <-chan struct{} {
	return w.done
}

func (w *withCancel) shutdown() {
	close(w.done)
}
