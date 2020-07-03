package funcs

import "testing"

func TestFunction(t *testing.T) {
	input := "md5sum($line)"
	fs, arg, err := NewFunctionStack(input)
	if err != nil {
		t.Errorf("error parsing function input '%s': %s (%v)\n", input, err.Error(), fs)
	}
	if arg != "$line" {
		t.Errorf("error parsing function input '%s': expected argument '$line' but got '%s' (%v)\n", input, arg, fs)
	}
	t.Log(input, fs, arg)

	result := fs.Call(input)
	if result != "b38699013d79e50d9d122433753959c1" {
		t.Errorf("error executing function stack '%s': expected result 'b38699013d79e50d9d122433753959c1' but got '%s' (%v)\n", input, result, fs)
	}

	input = "maskdigits(md5sum(maskdigits($line)))"
	fs, arg, err = NewFunctionStack(input)
	if err != nil {
		t.Errorf("error parsing function input '%s': %s (%v)\n", input, err.Error(), fs)
	}
	if arg != "$line" {
		t.Errorf("error parsing function input '%s': expected argument '$line' but got '%s' (%v)\n", input, arg, fs)
	}
	t.Log(input, fs, arg)

	result = fs.Call(input)
	if result != ".fac.bbe..bb.........d...a.c..b." {
		t.Errorf("error executing function stack '%s': expected result '.fac.bbe..bb.........d...a.c..b.' but got '%s' (%v)\n", input, result, fs)
	}

	input = "md5sum$line)"
	if fs, _, err := NewFunctionStack(input); err == nil {
		t.Errorf("Expected error parsing function input '%s' (%v) but got no error\n", input, fs)
	}

	input = "md5sum(makedigits$line))"
	if fs, _, err := NewFunctionStack(input); err == nil {
		t.Errorf("Expected error parsing function input '%s' (%v) but got no error\n", input, fs)
	}
}
