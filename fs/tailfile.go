package fs

import "sync"

// TailFile is to tail and filter a log file.
type TailFile struct {
	readFile
}

// NewTailFile returns a new file tailer.
func NewTailFile(filePath string, globID string, serverMessages chan<- string, limiter chan struct{}) TailFile {
	var mutex sync.Mutex

	return TailFile{
		readFile: readFile{
			filePath:       filePath,
			stop:           make(chan struct{}),
			globID:         globID,
			serverMessages: serverMessages,
			retry:          true,
			canSkipLines:   true,
			seekEOF:        true,
			limiter:        limiter,
			mutex:          &mutex,
		},
	}
}
