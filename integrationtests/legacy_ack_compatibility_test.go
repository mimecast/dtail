package integrationtests

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// This fixture models the protocol-4.1 server's old command accounting:
// an unknown .ack completes the only active command and closes normally,
// before a queued read can run. It deliberately uses a nonempty source so
// exit 0 with empty output cannot be mistaken for a valid empty query.
func TestLegacyAckServerCannotReportSuccessfulEmptyRead(t *testing.T) {
	skipIfNotIntegrationTest(t)
	for _, binary := range []string{"dcat", "dgrep"} {
		for _, wrongPin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrongPin=%v", binary, wrongPin), func(t *testing.T) {
				dir := t.TempDir()
				signer, keyPath := legacyFixtureKey(t, dir, "client")
				host, _ := legacyFixtureKey(t, dir, "host")
				pinned := host
				if wrongPin {
					pinned, _ = legacyFixtureKey(t, dir, "wrong-host")
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer closeIgnoringError(listener)
				address := listener.Addr().String()
				knownPath := filepath.Join(dir, "known_hosts")
				if err := os.WriteFile(knownPath, []byte(knownhosts.Line([]string{address}, pinned.PublicKey())+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				cfgPath := filepath.Join(dir, "client.json")
				cfg, err := json.Marshal(map[string]any{"Client": map[string]any{"KnownHostsPath": knownPath}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(dir, "source.log")
				if err := os.WriteFile(source, bytes.Repeat([]byte("fixture payload\n"), 75000), 0600); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() { result <- serveLegacyAckFixture(listener, host, signer.PublicKey(), source) }()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "../"+binary, "--cfg", cfgPath, "--logger", "stdout", "--logLevel", "error", "--plain", "--noColor", "--no-auth-key", "--auth-key-path", keyPath, "--user", "fixture", "--servers", address, "--files", source)
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("exit = %v, want 1; output: %s", err, output)
				}
				if ctx.Err() != nil {
					t.Fatalf("client timed out: %s", output)
				}
				if wrongPin {
					if strings.Contains(string(output), "unsupported DTail server") {
						t.Fatalf("reached protocol despite wrong host pin: %s", output)
					}
				} else if !strings.Contains(string(output), "missing capability escaped-framing-v1; please update DTail server") {
					t.Fatalf("missing actionable compatibility error: %s", output)
				}
				// Closing the listener also releases Accept if the client failed early.
				closeIgnoringError(listener)
				select {
				case serverErr := <-result:
					if !wrongPin && serverErr != nil {
						t.Fatal(serverErr)
					}
				case <-ctx.Done():
					t.Fatal("fixture did not finish")
				}
			})
		}
	}
}

func legacyFixtureKey(t *testing.T, dir, name string) (ssh.Signer, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return signer, path
}

func serveLegacyAckFixture(listener net.Listener, host ssh.Signer, authorized ssh.PublicKey, source string) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer closeIgnoringError(conn)
	if err := conn.SetDeadline(time.Now().Add(12 * time.Second)); err != nil {
		return err
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), authorized.Marshal()) {
			return nil, fmt.Errorf("unauthorized fixture key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(host)
	server, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return err
	}
	defer closeIgnoringError(server)
	go ssh.DiscardRequests(requests)
	// The client opens one session channel.
	incoming, ok := <-channels
	if !ok {
		return fmt.Errorf("no session channel")
	}
	channel, reqs, err := incoming.Accept()
	if err != nil {
		return err
	}
	defer closeIgnoringError(channel)
	for request := range reqs {
		if err := request.Reply(request.Type == "shell", nil); err != nil {
			return err
		}
		if request.Type == "shell" {
			break
		}
	}
	if _, err := io.WriteString(channel, protocol.HiddenCapabilitiesPrefix+protocol.CapabilityQueryUpdateV1+string(protocol.MessageDelimiter)); err != nil {
		return err
	}
	scanner := bufio.NewScanner(channel)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, ';'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 {
			return fmt.Errorf("invalid command frame %q", scanner.Text())
		}
		command, err := base64.StdEncoding.DecodeString(fields[3])
		if err != nil {
			return err
		}
		if strings.HasPrefix(string(command), ".ack") {
			_, err := io.WriteString(channel, ".syn close connection"+string(protocol.MessageDelimiter))
			if err != nil {
				return err
			}
			// Wait for the client to flush the close acknowledgement.
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), base64.StdEncoding.EncodeToString([]byte(".ack close connection"))) {
					return nil
				}
			}
			return scanner.Err()
		}
		if strings.HasPrefix(string(command), "cat") || strings.HasPrefix(string(command), "grep") {
			payload, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			_, err = channel.Write(payload)
			return err
		}
	}
	return scanner.Err()
}
