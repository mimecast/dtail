package clients

import (
	"io"
	"os"
	"slices"
)

// sessionNoFilesText says that an interactive session reads no files (no
// --files and no positional path) and how to give it some. Without files
// the background probe has nothing to sample, so "no field data yet" would
// suggest waiting for something that never comes. No default files are
// made up: the user names them.
const sessionNoFilesText = "the session reads no files: :reload --files PATH"

// sessionNoFilesRemoteTab and sessionNoFilesLocalTab follow
// sessionNoFilesText where the line editor completes paths in a --files
// value: the servers' paths, or in serverless mode the local ones.
const (
	sessionNoFilesRemoteTab = " (Tab lists remote paths)"
	sessionNoFilesLocalTab  = " (Tab lists local paths)"
)

// completesPaths reports whether w is the line editor's output, whose
// Tab completes paths; the plain control lines complete nothing.
func completesPaths(w io.Writer) bool {
	_, ok := w.(editorWriter)
	return ok
}

// stdinIsPipe reports whether the process's stdin is no terminal, which is
// when a serverless read of "-" reads it (readCommand.isInputFromPipe).
func stdinIsPipe() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

// readsNoFiles reports whether the session reads nothing: no file, and
// stdin ("-", which a serverless session without files gets) only where
// it is read, i.e. serverless with stdin not a terminal. Interactive dtail
// is the default exactly on a terminal, so there "-" reads nothing. False
// for a nil prober (no interactive session).
func (p *interactiveProber) readsNoFiles() bool {
	if p == nil {
		return false
	}
	files := p.files()
	if slices.ContainsFunc(files, func(file string) bool { return file != "" && file != probeStdin }) {
		return false
	}
	return !slices.Contains(files, probeStdin) || !p.serverless || !p.stdinPiped()
}

// noFilesHint is the hint for a session that reads no files, with the Tab
// note (remote or local paths by mode) when editor is set; empty when the
// session reads something (or for a nil prober).
func (p *interactiveProber) noFilesHint(editor bool) string {
	if !p.readsNoFiles() {
		return ""
	}
	if !editor {
		return sessionNoFilesText
	}
	if p.serverless {
		return sessionNoFilesText + sessionNoFilesLocalTab
	}
	return sessionNoFilesText + sessionNoFilesRemoteTab
}

// catalogEmptyHint is the hint of a completion that would offer catalog
// words while the catalog holds nothing: why nothing is there, in
// parentheses like the other completion hints; waiting when the session
// reads files and the probe has just not answered yet.
func catalogEmptyHint(snap catalogSnapshot, waiting string) string {
	if snap.NoFilesHint != "" {
		return "(" + snap.NoFilesHint + ")"
	}
	return waiting
}
