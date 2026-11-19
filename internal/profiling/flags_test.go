package profiling

import (
	"flag"
	"io"
	"testing"
)

func TestBindFlagsUsesProvidedFlagSet(t *testing.T) {
	fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var flags Flags
	BindFlags(fs, &flags)

	if err := fs.Parse([]string{"-profile", "-profiledir", "/tmp/profiles"}); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if !flags.Profile || flags.ProfileDir != "/tmp/profiles" {
		t.Fatalf("parsed flags = %+v", flags)
	}
}
