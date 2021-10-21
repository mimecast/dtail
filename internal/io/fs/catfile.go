package fs

// CatFile is for reading a whole file.
type CatFile struct {
	readFile
}

// NewCatFile returns a new file catter.
func NewCatFile(filePath string, globID string, serverMessages chan<- string,
	limiter chan struct{}) CatFile {

	return CatFile{
		readFile: readFile{
			filePath:       filePath,
			globID:         globID,
			serverMessages: serverMessages,
			retry:          false,
			canSkipLines:   false,
			seekEOF:        false,
			limiter:        limiter,
		},
	}
}
