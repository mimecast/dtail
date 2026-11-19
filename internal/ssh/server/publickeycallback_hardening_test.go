package server

import (
	"bytes"
	"testing"

	"github.com/mimecast/dtail/internal/logging"

	gossh "golang.org/x/crypto/ssh"
)

func TestValidUserName(t *testing.T) {
	for name, want := range map[string]bool{
		"alice":       true,
		"alice.smith": true,
		"":            false,
		".":           false,
		"..":          false,
		"../../tmp/x": false,
		"a/b":         false,
		`a\b`:         false,
		"a\x00b":      false,
	} {
		if got := validUserName(name); got != want {
			t.Errorf("validUserName(%q) = %v, want %v", name, got, want)
		}
	}
}

type warnCountingLogger struct {
	logging.NopLogger
	warns int
}

func (l *warnCountingLogger) Warn(args ...interface{}) string {
	l.warns++
	return ""
}

// Trailing comment and blank lines are no unparseable keys: they must not be
// warned about, and a key between them is still found.
func TestVerifyAuthorizedKeysIgnoresCommentAndBlankLines(t *testing.T) {
	user := testServerUser(t)
	key := testPublicKey(t, 51)

	var authorizedKeys bytes.Buffer
	authorizedKeys.WriteString("# header\n\n")
	authorizedKeys.Write(gossh.MarshalAuthorizedKey(key))
	for i := 0; i < 2000; i++ {
		authorizedKeys.WriteString("# trailing comment\n\n")
	}

	logger := &warnCountingLogger{}
	if _, err := verifyAuthorizedKeys(user, authorizedKeys.Bytes(), key, logger); err != nil {
		t.Fatalf("verifyAuthorizedKeys failed: %v", err)
	}
	if logger.warns != 0 {
		t.Errorf("expected no warning, got %d", logger.warns)
	}
}
