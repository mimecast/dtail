package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestEncodeReadPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantEncoded bool
	}{
		{name: "plain path", path: "/var/log/app.log"},
		{name: "glob", path: "/var/log/*.log"},
		{name: "stdin", path: "-"},
		{name: "journal", path: "journal:ssh.service"},
		{name: "tab and percent", path: "/var/log/a\tb%20c.log"},
		{name: "prefix inside", path: "/var/log/base64%x.log"},
		{name: "space", path: "/var/log/my app/a.log", wantEncoded: true},
		{name: "leading space", path: " a.log", wantEncoded: true},
		{name: "space glob", path: "/var/log/my app/*.log", wantEncoded: true},
		{name: "journal with space", path: "journal:my unit.service", wantEncoded: true},
		{name: "prefix", path: EncodedReadPathPrefix + "x.log", wantEncoded: true},
		{name: "non-ASCII", path: "/var/log/ä ö.log", wantEncoded: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			word := EncodeReadPath(tc.path)
			if got := ReadPathNeedsEncoding(tc.path); got != tc.wantEncoded {
				t.Fatalf("ReadPathNeedsEncoding(%q) = %v, want %v", tc.path, got, tc.wantEncoded)
			}
			if !tc.wantEncoded && word != tc.path {
				t.Fatalf("EncodeReadPath(%q) = %q, want it verbatim", tc.path, word)
			}
			if tc.wantEncoded && !strings.HasPrefix(word, EncodedReadPathPrefix) {
				t.Fatalf("EncodeReadPath(%q) = %q, want prefix %q", tc.path, word, EncodedReadPathPrefix)
			}
			if strings.Contains(word, " ") {
				t.Fatalf("EncodeReadPath(%q) = %q contains a space", tc.path, word)
			}
			decoded, err := DecodeReadPath(word)
			if err != nil || decoded != tc.path {
				t.Fatalf("DecodeReadPath(%q) = %q, %v; want %q", word, decoded, err, tc.path)
			}
		})
	}
}

func TestDecodeReadPathRejectsInvalidWords(t *testing.T) {
	for _, word := range []string{
		EncodedReadPathPrefix,
		EncodedReadPathPrefix + "!!!",
		EncodedReadPathPrefix + "YQ", // missing padding
		EncodedReadPathPrefix + "YQ=*",
	} {
		t.Run(word, func(t *testing.T) {
			if path, err := DecodeReadPath(word); !errors.Is(err, ErrInvalidEncodedReadPath) {
				t.Fatalf("DecodeReadPath(%q) = %q, %v; want %v", word, path, err, ErrInvalidEncodedReadPath)
			}
		})
	}
}
