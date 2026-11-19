package config

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
)

func TestNewReadShareIsRandomPerGroup(t *testing.T) {
	first, err := NewReadShare(3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewReadShare(3)
	if err != nil {
		t.Fatal(err)
	}
	if first.Group == second.Group {
		t.Errorf("two groups got the same ID %q", first.Group)
	}
	if len(first.Group) != 2*readShareIDBytes || first.Members != 3 {
		t.Errorf("NewReadShare(3) = %+v", first)
	}
	if _, err := ParseReadShare(first.String()); err != nil {
		t.Errorf("ParseReadShare(%q) = %v", first.String(), err)
	}
	for _, members := range []int{0, -1, maxReadShareMembers + 1} {
		if _, err := NewReadShare(members); err == nil {
			t.Errorf("NewReadShare(%d) did not fail", members)
		}
	}
}

func TestParseReadShare(t *testing.T) {
	tests := []struct {
		value   string
		want    ReadShare
		wantErr bool
	}{
		{value: "abc123:2", want: ReadShare{Group: "abc123", Members: 2}},
		{value: "a-b_C:1", want: ReadShare{Group: "a-b_C", Members: 1}},
		{value: "abc", wantErr: true},
		{value: ":2", wantErr: true},
		{value: "abc:", wantErr: true},
		{value: "abc:0", wantErr: true},
		{value: "abc:x", wantErr: true},
		{value: "abc:2:3", wantErr: true},
		{value: "a/b:2", wantErr: true},
		{value: strings.Repeat("a", maxReadShareIDLength+1) + ":2", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseReadShare(tt.value)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseReadShare(%q) error = %v, want error %v", tt.value, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseReadShare(%q) = %+v, want %+v", tt.value, got, tt.want)
		}
	}
}

func TestReadShareOptionRoundTripsAndLeavesOtherOptionsAlone(t *testing.T) {
	share := ReadShare{Group: "0123abcd", Members: 4}
	args := Args{ReadShare: share, LContext: lcontext.LContext{MaxCount: 2}, Quiet: true}
	serialized := args.SerializeOptions()

	// The session spec validator and the command dispatcher split on ':'.
	options, ltx, err := DeserializeOptions(strings.Split(serialized, ":"))
	if err != nil {
		t.Fatalf("DeserializeOptions(%q) = %v", serialized, err)
	}
	if ltx.MaxCount != 2 || options["quiet"] != "true" {
		t.Errorf("other options changed: %+v %v", ltx, options)
	}
	// A dserver without shared reads keeps the unknown option in the map
	// and ignores it.
	got, err := ParseReadShare(options[ReadShareOption])
	if err != nil || got != share {
		t.Errorf("share option = %q (%+v, %v), want %+v", options[ReadShareOption], got, err, share)
	}

	if serialized := (&Args{}).SerializeOptions(); strings.Contains(serialized, ReadShareOption) {
		t.Errorf("Args without a share serialized %q", serialized)
	}
}

func TestReadShareStaysOutOfLogs(t *testing.T) {
	share := ReadShare{Group: "secretgroupid", Members: 3}
	args := Args{ReadShare: share, Quiet: true}
	command := "map:" + args.SerializeOptions() + " from STATS select count($line)"
	encoded := strings.TrimPrefix(strings.SplitN(strings.SplitN(command, "share=", 2)[1], " ", 2)[0], "base64%")

	logged := map[string]string{
		"args":     args.String(),
		"redacted": share.Redacted(),
		"command":  RedactReadShare(command),
		"commands": strings.Join(RedactReadShares([]string{command, "cat:" + args.SerializeOptions() + " /f"}), " "),
	}
	for name, line := range logged {
		if strings.Contains(line, share.Group) || strings.Contains(line, encoded) {
			t.Errorf("%s log %q has the group ID", name, line)
		}
	}
	if got := RedactReadShare(command); got != "map:quiet=true:share=REDACTED from STATS select count($line)" &&
		got != "map:share=REDACTED:quiet=true from STATS select count($line)" {
		t.Errorf("RedactReadShare() = %q", got)
	}
	if got := share.Redacted(); got != "REDACTED:3" {
		t.Errorf("Redacted() = %q, want REDACTED:3", got)
	}
	if got := (ReadShare{}).Redacted(); got != "none" {
		t.Errorf("Redacted() of no share = %q, want none", got)
	}
	if plain := "cat:max=1:quiet=true /var/log/share=x.log"; RedactReadShare(plain) != plain {
		t.Errorf("RedactReadShare(%q) = %q, want it unchanged", plain, RedactReadShare(plain))
	}
}
