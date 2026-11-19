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
