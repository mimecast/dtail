package protocol

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestProbeRequestRoundTrip(t *testing.T) {
	request := ProbeRequest{ID: "p-1.x_Y", Files: []string{"/var/log/a.log", EncodeReadPath("/a b/*.log")}}
	command, err := EncodeProbeCommand(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	name, argument, ok := strings.Cut(command, " ")
	if !ok || name != ProbeCommand || strings.Contains(argument, " ") {
		t.Fatalf("command %q is not %q followed by one word", command, ProbeCommand)
	}
	decoded, err := DecodeProbeRequest(argument)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.ID != request.ID || strings.Join(decoded.Files, ",") != strings.Join(request.Files, ",") {
		t.Fatalf("decoded %+v, want %+v", decoded, request)
	}
}

func TestDecodeProbeRequestRejectsInvalidInput(t *testing.T) {
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := map[string]string{
		"not base64":   "!!!",
		"not json":     encode("{"),
		"wrong type":   encode(`{"id":"a","files":"x"}`),
		"missing id":   encode(`{"files":["x"]}`),
		"blank in id":  encode(`{"id":"a b","files":["x"]}`),
		"unknown id":   encode(`{"id":"-","files":["x"]}`),
		"long id":      encode(`{"id":"` + strings.Repeat("a", ProbeMaxIDBytes+1) + `","files":["x"]}`),
		"newline id":   encode(`{"id":"a\n","files":["x"]}`),
		"non-ascii id": encode(`{"id":"ä","files":["x"]}`),
	}
	for name, argument := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProbeRequest(argument); !errors.Is(err, ErrInvalidProbe) {
				t.Fatalf("DecodeProbeRequest(%q) error = %v, want ErrInvalidProbe", argument, err)
			}
		})
	}
}

func TestValidProbeID(t *testing.T) {
	for id, want := range map[string]bool{
		"a": true, "A-9._z": true, strings.Repeat("x", ProbeMaxIDBytes): true,
		"": false, ProbeUnknownID: false, "a b": false, "a/b": false,
		strings.Repeat("x", ProbeMaxIDBytes+1): false,
	} {
		if got := ValidProbeID(id); got != want {
			t.Errorf("ValidProbeID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestValidProbeTable(t *testing.T) {
	for table, want := range map[string]bool{
		"STATS": true, "a_b-c:d": true, "ÄÖÜ": true, strings.Repeat("T", ProbeMaxTableBytes): true,
		"": false, "A B": false, "A\tB": false, "A\x00": false, "A\x1b[2J": false,
		"A\u202eB": false, "\xff": false, strings.Repeat("T", ProbeMaxTableBytes+1): false,
		"A|B": false, "STATS|x=1": false, "A,B": false, `A"B`: false, "A'B": false, "A`B": false,
	} {
		if got := ValidProbeTable(table); got != want {
			t.Errorf("ValidProbeTable(%q) = %v, want %v", table, got, want)
		}
	}
}

func TestTableProbeRoundTrip(t *testing.T) {
	command, err := EncodeProbeCommand(ProbeRequest{ID: "t1", Files: []string{"a.log"}, Table: "STATS"})
	if err != nil {
		t.Fatal(err)
	}
	_, argument, _ := strings.Cut(command, " ")
	decoded, err := DecodeProbeRequest(argument)
	if err != nil || decoded.Table != "STATS" {
		t.Fatalf("decoded %+v, %v; want table STATS", decoded, err)
	}
	data := []byte(`{"id":"t1","table":"STATS","formats":{"default":{"tables":{"STATS":{"a":[]}},` +
		`"tableVariables":{"STATS":{"$caller":["x.go:1"]}}}},"filesSampled":1,"linesSampled":1}`)
	answer, err := DecodeProbeAnswer(EncodeProbeAnswer("t1", data))
	if err != nil {
		t.Fatal(err)
	}
	if answer.Table != "STATS" || answer.Formats["default"].TableVariables["STATS"]["$caller"][0] != "x.go:1" {
		t.Fatalf("decoded %+v", answer)
	}
}

func TestProbeAnswerRoundTrip(t *testing.T) {
	data := []byte(`{"id":"p1","defaultLogFormat":"default","formats":{"default":{"tables":` +
		`{"STATS":{"a":["1"]}}}},"filesSampled":1,"linesSampled":2}`)
	frame := EncodeProbeAnswer("p1", data)
	if !strings.HasPrefix(frame, HiddenProbePrefix) || !IsHiddenMessage(frame) {
		t.Fatalf("frame %q is no hidden probe frame", frame)
	}
	answer, err := DecodeProbeAnswer(frame + "\n")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer.ID != "p1" || answer.LinesSampled != 2 ||
		answer.Formats["default"].Tables["STATS"]["a"][0] != "1" {
		t.Fatalf("decoded %+v", answer)
	}
}

func TestDecodeProbeAnswerRejectsInvalidFrames(t *testing.T) {
	valid := EncodeProbeAnswer("p1", []byte(`{"id":"p1"}`))
	tests := map[string]string{
		"no prefix":    ".syn other p1 e30=",
		"no answer":    HiddenProbePrefix + "p1",
		"not base64":   HiddenProbePrefix + "p1 !!!",
		"id mismatch":  EncodeProbeAnswer("p2", []byte(`{"id":"p1"}`)),
		"not json":     EncodeProbeAnswer("p1", []byte(`{`)),
		"cut answer":   valid[:len(valid)-2],
		"empty prefix": "",
	}
	for name, frame := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProbeAnswer(frame); !errors.Is(err, ErrInvalidProbe) {
				t.Fatalf("DecodeProbeAnswer(%q) error = %v, want ErrInvalidProbe", frame, err)
			}
		})
	}
}
