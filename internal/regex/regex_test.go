package regex

import (
	"strings"
	"testing"
)

func TestZeroValueRegexDoesNotPanic(t *testing.T) {
	var r Regex

	if got := r.Match([]byte("anything")); !got {
		t.Fatalf("zero-value Regex should behave like a no-op match, got %v", got)
	}

	if got := r.MatchString("anything"); !got {
		t.Fatalf("zero-value Regex should behave like a no-op match, got %v", got)
	}
}

func TestRegex(t *testing.T) {
	input := "hello"

	r := NewNoop()
	if !r.MatchString(input) {
		t.Errorf("expected to match string '%s' with noop regex '%v' but didn't\n",
			input, r)
	}

	r, err := New(".hello", Default)
	if err != nil {
		t.Errorf("unable to create regex: %v\n", err)
	}
	if r.MatchString(input) {
		t.Errorf("expected to match string '%s' with regex '%v' but didn't\n",
			input, r)
	}

	serialized, err := r.Serialize()
	if err != nil {
		t.Errorf("unable to serialize regex: %v: %v\n", serialized, err)
	}
	r2, err := Deserialize(serialized)
	if err != nil {
		t.Errorf("unable to serialize deserialized regex: %v: %v\n", serialized, err)
	}
	if r.String() != r2.String() {
		t.Errorf("regex should be the same after deserialize(serialize(..)), got "+
			"'%s' but expected '%s'.\n", r2.String(), r.String())
	}

	r, err = New(".hello", Invert)
	if err != nil {
		t.Errorf("unable to create regex: %v\n", err)
	}
	if !r.MatchString(input) {
		t.Errorf("expected to not match string '%s' with regex '%v' but matched\n",
			input, r)
	}

	serialized, err = r.Serialize()
	if err != nil {
		t.Errorf("unable to serialize regex: %v: %v\n", serialized, err)
	}
	r2, err = Deserialize(serialized)
	if err != nil {
		t.Errorf("unable to serialize deserialized regex: %v: %v\n", serialized, err)
	}
	if r.String() != r2.String() {
		t.Errorf("regex should be the same after deserialize(serialize(..)), got "+
			"'%s' but expected '%s'.\n", r2.String(), r.String())
	}
}

func TestDeserializeRejectsUnknownFlags(t *testing.T) {
	t.Parallel()

	if _, err := Deserialize("regex:invert,bogus foo"); err == nil {
		t.Fatal("expected Deserialize to reject unknown regex flags")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected error to mention the unknown flag, got %v", err)
	}
}

// TestSerializeRoundTripAllFlags pins Serialize/Deserialize for every flag on
// both initialized and uninitialized regexes: Serialize must either fail or
// emit a string which Deserialize turns back into an equivalent regex.
func TestSerializeRoundTripAllFlags(t *testing.T) {
	t.Parallel()

	mustNew := func(pattern string, flag Flag) Regex {
		t.Helper()
		r, err := New(pattern, flag)
		if err != nil {
			t.Fatalf("New(%q, %v) error = %v", pattern, flag, err)
		}
		return r
	}

	tests := []struct {
		name string
		re   Regex
		// wantWire is the exact serialization; empty means Serialize errors.
		wantWire string
		// wantErr must be part of the Serialize error when wantWire is empty.
		wantErr string
		// matchInput is checked against the original and the round-tripped
		// regex, which must agree with wantMatch.
		matchInput string
		wantMatch  bool
	}{
		{name: "initialized undefined", re: mustNew("hel+o", Undefined),
			wantErr: `flag "undefined"`},
		{name: "initialized default", re: mustNew("hel+o", Default),
			wantWire: "regex:default hel+o", matchInput: "say helllo", wantMatch: true},
		{name: "initialized invert", re: mustNew("hel+o", Invert),
			wantWire: "regex:invert hel+o", matchInput: "say helllo", wantMatch: false},
		{name: "initialized noop", re: mustNew("hel+o", Noop),
			wantWire: "regex:noop hel+o", matchInput: "nothing", wantMatch: true},
		{name: "initialized literal default", re: mustNew("hello", Default),
			wantWire: "regex:default,literal hello", matchInput: "hello world", wantMatch: true},
		{name: "NewNoop", re: NewNoop(),
			wantWire: "regex:noop ", matchInput: "anything", wantMatch: true},
		{name: "New empty pattern with undefined flag is noop", re: mustNew("", Undefined),
			wantWire: "regex:noop ", matchInput: "anything", wantMatch: true},
		{name: "out of range flag", re: mustNew("hel+o", Flag(42)),
			wantErr: `flag "undefined"`},
		{name: "initialized without flags",
			re:      Regex{regexStr: "hel+o", initialized: true},
			wantErr: "without a flag"},
		{name: "uninitialized undefined",
			re:      Regex{regexStr: "hel+o", flags: []Flag{Undefined}},
			wantErr: "not initialized"},
		{name: "uninitialized default",
			re:      Regex{regexStr: "hel+o", flags: []Flag{Default}},
			wantErr: "not initialized"},
		{name: "uninitialized invert",
			re:      Regex{regexStr: "hel+o", flags: []Flag{Invert}},
			wantErr: "not initialized"},
		{name: "uninitialized noop",
			re:      Regex{regexStr: "hel+o", flags: []Flag{Noop}},
			wantErr: "not initialized"},
		{name: "zero value", re: Regex{}, wantErr: "not initialized"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wire, err := tt.re.Serialize()
			if tt.wantWire == "" {
				if err == nil {
					t.Fatalf("Serialize() = %q, want error containing %q", wire, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Serialize() error = %v, want it to contain %q", err, tt.wantErr)
				}
				if wire != "" {
					t.Fatalf("Serialize() returned %q alongside an error", wire)
				}
				return
			}
			if err != nil {
				t.Fatalf("Serialize() error = %v", err)
			}
			if wire != tt.wantWire {
				t.Fatalf("Serialize() = %q, want %q", wire, tt.wantWire)
			}

			got, err := Deserialize(wire)
			if err != nil {
				t.Fatalf("Deserialize(%q) error = %v", wire, err)
			}
			// The literal hint is not compared: NewNoop leaves it unset while
			// the receiver derives it from the (empty) pattern. Both only
			// affect how a match is computed, not its result.
			if got.Pattern() != tt.re.Pattern() || !got.initialized ||
				len(got.flags) != len(tt.re.flags) || got.flags[0] != tt.re.flags[0] {
				t.Fatalf("Deserialize(%q) = %v, want %v", wire, got, tt.re)
			}
			again, err := got.Serialize()
			if err != nil {
				t.Fatalf("re-Serialize() error = %v", err)
			}
			if twice, err := Deserialize(again); err != nil ||
				twice.Pattern() != got.Pattern() || twice.flags[0] != got.flags[0] {
				t.Fatalf("second round trip of %q = %v, %v; want %v", again, twice, err, got)
			}
			if m := tt.re.MatchString(tt.matchInput); m != tt.wantMatch {
				t.Fatalf("original MatchString(%q) = %t, want %t", tt.matchInput, m, tt.wantMatch)
			}
			if m := got.MatchString(tt.matchInput); m != tt.wantMatch {
				t.Fatalf("round-tripped MatchString(%q) = %t, want %t", tt.matchInput, m, tt.wantMatch)
			}
		})
	}
}

// TestNewFlagWireNames pins which flag names the wire protocol accepts: every
// name Serialize can emit, and nothing else — in particular not "undefined".
func TestNewFlagWireNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    Flag
		wantErr bool
	}{
		{name: "default", want: Default},
		{name: "invert", want: Invert},
		{name: "noop", want: Noop},
		{name: "undefined", want: Undefined, wantErr: true},
		{name: "", want: Undefined, wantErr: true},
		{name: "literal", want: Undefined, wantErr: true},
	}

	for _, tt := range tests {
		got, err := NewFlag(tt.name)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("NewFlag(%q) = %v, %v; want %v, error %t", tt.name, got, err, tt.want, tt.wantErr)
		}
		if !tt.wantErr && (!got.serializable() || got.String() != tt.name) {
			t.Errorf("flag %v: serializable() = %t, String() = %q", got, got.serializable(), got.String())
		}
	}
	if Undefined.serializable() {
		t.Error("Undefined must not be serializable")
	}
}
