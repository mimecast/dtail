package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ReadShareOption is the read command option that carries a ReadShare. A
// dserver that does not know the option ignores it and reads privately.
const ReadShareOption = "share"

const (
	// readShareIDBytes is the number of random bytes of a group ID.
	readShareIDBytes = 16
	// maxReadShareIDLength bounds the group ID a client may send.
	maxReadShareIDLength = 64
	// maxReadShareMembers bounds the member count a client may send.
	maxReadShareMembers = 10000
)

// ReadShare asks dserver to read a file once for a group of sessions that
// start the same one-shot read together, such as the scheduled jobs of one
// dserver that run at the same time on the same files. Only those jobs set
// it; interactive clients never do. The group ID grants no access: every
// session still reads only what it may read.
type ReadShare struct {
	// Group is the group's random ID.
	Group string
	// Members is how many sessions of the group read the same files.
	Members int
}

// NewReadShare returns a ReadShare for members sessions with a new random,
// unguessable group ID.
func NewReadShare(members int) (ReadShare, error) {
	if members < 1 || members > maxReadShareMembers {
		return ReadShare{}, fmt.Errorf("read share member count %d out of range 1..%d",
			members, maxReadShareMembers)
	}
	id := make([]byte, readShareIDBytes)
	if _, err := rand.Read(id); err != nil {
		return ReadShare{}, fmt.Errorf("read share group ID: %w", err)
	}
	return ReadShare{Group: hex.EncodeToString(id), Members: members}, nil
}

// ParseReadShare parses the value of the ReadShareOption, "<group>:<members>".
func ParseReadShare(value string) (ReadShare, error) {
	group, count, found := strings.Cut(value, ":")
	if !found {
		return ReadShare{}, fmt.Errorf("read share %q: want <group>:<members>", value)
	}
	if err := validateReadShareGroup(group); err != nil {
		return ReadShare{}, err
	}
	members, err := strconv.Atoi(count)
	if err != nil {
		return ReadShare{}, fmt.Errorf("read share member count %q: %w", count, err)
	}
	if members < 1 || members > maxReadShareMembers {
		return ReadShare{}, fmt.Errorf("read share member count %d out of range 1..%d",
			members, maxReadShareMembers)
	}
	return ReadShare{Group: group, Members: members}, nil
}

// Redacted returns s for logs: its member count, without the group ID.
func (s ReadShare) Redacted() string {
	if s.IsZero() {
		return "none"
	}
	return fmt.Sprintf("%s:%d", redacted, s.Members)
}

// redacted replaces a read share's group ID in logs.
const redacted = "REDACTED"

// readShareOptionPattern matches a serialized ReadShareOption with its value
// in a command's options.
var readShareOptionPattern = regexp.MustCompile(`(^|[: ])` + regexp.QuoteMeta(ReadShareOption) + `=[^: ]*`)

// RedactReadShare returns text, such as a command sent to dserver, with the
// value of every ReadShareOption in it, which holds the group ID, redacted.
func RedactReadShare(text string) string {
	return readShareOptionPattern.ReplaceAllString(text, "${1}"+ReadShareOption+"="+redacted)
}

// RedactReadShares returns RedactReadShare of each of texts.
func RedactReadShares(texts []string) []string {
	redacted := make([]string, len(texts))
	for i, text := range texts {
		redacted[i] = RedactReadShare(text)
	}
	return redacted
}

// IsZero reports whether s asks for no shared read.
func (s ReadShare) IsZero() bool {
	return s == ReadShare{}
}

// String returns the value of the ReadShareOption for s.
func (s ReadShare) String() string {
	return fmt.Sprintf("%s:%d", s.Group, s.Members)
}

func validateReadShareGroup(group string) error {
	if group == "" || len(group) > maxReadShareIDLength {
		return errors.New("read share group ID must have 1 to 64 characters")
	}
	for _, r := range group {
		if !isReadShareGroupRune(r) {
			return fmt.Errorf("read share group ID %q has a character other than [A-Za-z0-9_-]", group)
		}
	}
	return nil
}

func isReadShareGroupRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return true
	default:
		return false
	}
}
