package handlers

// isEmptyServerMessage reports whether message, a server message without its
// generation prefix, carries no text, so that it is not passed on to the
// client in any output mode: a framed client would print a bare
// "SERVER|<host>|" line for it. A diagnostic's text never depends on the
// server's log level (see logging.WarnMessage), so no diagnostic is empty;
// this guards any other sender.
func isEmptyServerMessage(message string) bool {
	return message == "" || message == "\n"
}
