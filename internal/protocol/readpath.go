package protocol

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// EncodedReadPathPrefix marks the path word of a read command
// ("<mode>:<options> <path> <regex>") as base64 encoded. It is the prefix
// config.Args already uses for option values that would break the option
// syntax.
//
// Servers split every command on single spaces and take the second word as
// the path, so a path containing a space would be cut there and the rest
// taken as the regex. EncodeReadPath therefore encodes such paths; base64
// (standard alphabet) contains no space. Every other path is sent verbatim,
// so the commands of space-free paths stay byte-identical to those of older
// clients and keep working with older servers.
const EncodedReadPathPrefix = "base64%"

// ErrInvalidEncodedReadPath reports a read command path word that carries
// EncodedReadPathPrefix but does not decode to a non-empty path.
var ErrInvalidEncodedReadPath = errors.New("invalid encoded read path")

// ReadPathNeedsEncoding reports whether path must be sent encoded: it
// contains a space, which would split it, or it starts with
// EncodedReadPathPrefix, which a server would otherwise take as encoded.
// Servers that do not advertise CapabilityEncodedReadPathV1 cannot read such
// paths.
func ReadPathNeedsEncoding(path string) bool {
	return strings.Contains(path, " ") || strings.HasPrefix(path, EncodedReadPathPrefix)
}

// EncodeReadPath returns the path word of a read command for path: path
// itself, or EncodedReadPathPrefix followed by its base64 encoding when
// ReadPathNeedsEncoding reports true.
func EncodeReadPath(path string) string {
	if !ReadPathNeedsEncoding(path) {
		return path
	}
	return EncodedReadPathPrefix + base64.StdEncoding.EncodeToString([]byte(path))
}

// DecodeReadPath returns the path a read command path word stands for. A
// word without EncodedReadPathPrefix is the path itself (all older clients
// send that); a word with it must decode to a non-empty path, else it fails
// with ErrInvalidEncodedReadPath and the command must not read any file.
func DecodeReadPath(word string) (string, error) {
	encoded, ok := strings.CutPrefix(word, EncodedReadPathPrefix)
	if !ok {
		return word, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEncodedReadPath, err)
	}
	if len(decoded) == 0 {
		return "", fmt.Errorf("%w: empty path", ErrInvalidEncodedReadPath)
	}
	return string(decoded), nil
}
