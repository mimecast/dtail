package loggers

import (
	"context"
	"sync"
	"time"
)

type fout struct {
	file   *file
	stdout *stdout
}

// Logs to both, a file and stdout
func newFout() *fout {
	return &fout{file: newFile(), stdout: newStdout()}
}

func (f *fout) Start(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		defer wg.Done()

		var wg2 sync.WaitGroup
		wg2.Add(2)
		f.file.Start(ctx, &wg2)
		f.stdout.Start(ctx, &wg2)
		wg2.Wait()
	}()
}

func (f *fout) Log(now time.Time, message string) {
	f.stdout.Log(now, message)
	f.file.Log(now, message)
}

func (f *fout) LogWithColors(now time.Time, message, coloredMessage string) {
	f.stdout.LogWithColors(now, "", coloredMessage)
	f.file.Log(now, message)
}

func (f *fout) Flush()  { f.stdout.Flush(); f.file.Flush() }
func (f *fout) Pause()  { f.stdout.Pause(); f.file.Pause() }
func (f *fout) Resume() { f.stdout.Resume(); f.file.Resume() }
func (f *fout) Rotate() { f.file.Rotate() }

func (fout) SupportsColors() bool { return true }
