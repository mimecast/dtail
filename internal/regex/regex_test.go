package regex

import "testing"

func TestRegex(t *testing.T) {

    input := "hello"
    r, err := New(".hello", Default)
    if err != nil {
		t.Errorf("error: unable to create regex: %v\n", err)
    }

    if r.MatchString(input) {
		t.Errorf("error: expected to match string '%s' with regex '%v' but didnt\n", input, r)
    }
}
