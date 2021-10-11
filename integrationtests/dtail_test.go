package integrationtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TODO: Have a serverless variant too.
func TestDTailWithServer(t *testing.T) {
	followFile := "dtail.follow.tmp"
	greetings := []string{"world!", "sol-system!", "milky-way!", "universe!", "multiverse!"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-time.After(time.Minute):
			t.Error("Max time for this test exceeded!")
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	serverCh, _, _, err := startCommand(ctx,
		"../dserver",
		"--logger", "stdout",
		"--logLevel", "info",
		"--port", "4242",
		"--relaxedAuth",
	)
	if err != nil {
		t.Error(err)
		return
	}

	clientCh, _, _, err := startCommand(ctx,
		"../dtail",
		"--logger", "stdout",
		"--logLevel", "devel",
		"--servers", "localhost:4242",
		"--files", followFile,
		"--grep", "Hello",
		"--trustAllHosts",
		"--noColor",
	)
	if err != nil {
		t.Error(err)
		return
	}
	// Write greetings to followFile
	fd, err := os.Create(followFile)
	if err != nil {
		t.Error(err)
	}
	defer fd.Close()

	go func() {
		var circular int
		for {
			select {
			case <-time.After(time.Second):
				fd.WriteString(time.Now().String())
				fd.WriteString(fmt.Sprintf(" - Hello %s\n", greetings[circular]))
				circular = (circular + 1) % len(greetings)
			case <-ctx.Done():
				return
			}
		}
	}()

	var greetingsRecv []string

	for len(greetingsRecv) < len(greetings) {
		select {
		case line := <-serverCh:
			t.Log("server:", line)
		case line := <-clientCh:
			t.Log("client:", line)
			if strings.Contains(line, "Hello ") {
				s := strings.Split(line, " ")
				greeting := s[len(s)-1]
				greetingsRecv = append(greetingsRecv, greeting)
				t.Log("Received greeting", greeting, len(greetingsRecv))
			}
		case <-ctx.Done():
			t.Log("Done reading client and server pipes")
		}
	}

	// We expect to have received the greetings in the same order they were sent.`
	offset := -1
	for i, g := range greetings {
		if g == greetingsRecv[0] {
			offset = i
			break
		}
	}
	if offset == -1 {
		t.Error("Could not find first offset of greetings received")
		return
	}

	for i, g := range greetingsRecv {
		index := (i + offset) % len(greetings)
		if greetings[index] != g {
			t.Error(fmt.Sprintf("Expected '%s' but got '%s' at '%v' vs '%v'\n",
				g, greetings[index], greetings, greetingsRecv))
			return
		}
	}

	os.Remove(followFile)
}

func TestDTailColorTable(t *testing.T) {
	stdoutFile := "dtailcolortable.stdout.tmp"
	expectedStdoutFile := "dtailcolortable.expected"

	_, err := runCommand(context.TODO(), stdoutFile, "../dtail", "--colorTable")
	if err != nil {
		t.Error(err)
		return
	}
	if err := compareFiles(t, stdoutFile, expectedStdoutFile); err != nil {
		t.Error(err)
		return
	}
	os.Remove(stdoutFile)
}
