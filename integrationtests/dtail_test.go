package integrationtests

import (
	"context"
	"os"
	"testing"
)

func TestDTailWithServer(t *testing.T) {
	followFile := "dtail.follow.tmp"
	//serverStdoutFile := "dtail.dserver.stdout.tmp"
	//greetings := []string{"world", "sol system", "milky way", "universe", "multiverse"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	for {
		select {
		case line := <-serverCh:
			t.Log("server:", line)
		case line := <-clientCh:
			t.Log("client:", line)
		case <-ctx.Done():
			t.Log("Done reading client and server pipes")
		}
	}

	/*
		// Start dtail client, connect to the server and follow followFile.

		//clientStdoutFile := "dtail.stdout.tmp"
		/*

			t.Log(clientArgs)
			// TODO: Pipe with dtail command to read stdin stream.
			//		runCommandContextRetry(ctx, t, "../dtail", clientArgs, clientStdoutFile)

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
					case <-ctx.Done():
						return
					case <-time.After(time.Second):
						fd.WriteString(time.Now().String())
						fd.WriteString(fmt.Sprintf(" - Hello %s!\n", greetings[circular]))
						circular = (circular + 1) % len(greetings)
					}
				}
			}()
	*/

	/*
		os.Remove(serverStdoutFile)
		os.Remove(clientStdoutFile)
		os.Remove(followFile)
	*/
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
