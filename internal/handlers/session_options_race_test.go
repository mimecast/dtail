package handlers

import (
	"bytes"
	"testing"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

// TestServerHandlerSessionOptionsDoNotRaceWithOutput reproduces, under go
// test -race, the interleavings in which the session's quiet/plain options
// are set while other goroutines of the session already read them:
//
//   - A server message queued before the first command carrying options
//     (here the AUTHKEY reply) is framed by the output goroutine
//     (sessionFramer.readServerMessage) while the command input goroutine
//     handles that next command (commandDispatcher.handleOptions sets the
//     plain mode). The AUTHKEY reply is a marked control frame, whose
//     framing does not depend on the plain mode.
//   - A read command without options, started before one carrying options,
//     builds its line writer on its own goroutine (newReadLineWriter reads
//     the plain mode) while the command input goroutine handles the later
//     command.

func TestServerHandlerSessionOptionsDoNotRaceWithOutput(t *testing.T) {
	path := writeControlLookalikeFile(t)
	unoptioned, err := session.Spec{Mode: omode.CatClient, Files: []string{path}, Regex: "."}.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	cases := map[string][]string{
		"server message before options": {"AUTHKEY abc"},
		"read command before options":   unoptioned,
	}
	for name, before := range cases {
		t.Run(name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runCatSessionAfter(t, handler, path, "plain=true", before...)
			if !handler.plainMode() {
				t.Fatal("plain option of the cat command was not applied")
			}
			if !bytes.HasSuffix(stream, []byte(".syn close connection\xac")) {
				t.Fatalf("session ended without the close handshake: %q", stream)
			}
			if name == "server message before options" &&
				!bytes.Contains(stream, []byte("AUTHKEY ERR")) {
				t.Fatalf("no AUTHKEY reply in the session output: %q", stream)
			}
		})
	}
}
