package regex

import (
	"testing"
)

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
