package fs

// TailFile is to tail and filter a log file.
type TailFile struct {
	readFile
}

// NewTailFile returns a new file tailer.
func NewTailFile(filePath string, globID string, serverMessages chan<- string) TailFile {
	return TailFile{
		readFile: readFile{
			filePath:       filePath,
			globID:         globID,
			serverMessages: serverMessages,
			retry:          true,
			canSkipLines:   true,
			seekEOF:        true,
		},
	}
}
