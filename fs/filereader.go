package fs

// FileReader is the interface used on the dtail server to read/cat/grep/mapr... a file.
type FileReader interface {
	Start(lines chan<- LineRead, regex string) error
	FilePath() string
	Retry() bool
	Stop()
}
