package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
)

func TestSerializeOptionsUsesStableOrdering(t *testing.T) {
	args := Args{
		LContext:   lcontextForTest(3, 1, 2),
		Plain:      true,
		Quiet:      true,
		Serverless: true,
	}

	got := args.SerializeOptions()
	want := "after=2:before=1:max=3:plain=true:quiet=true:serverless=true"
	if got != want {
		t.Fatalf("unexpected serialized options:\nwant %q\ngot  %q", want, got)
	}
}

func TestSerializeOptionsRoundTripsReservedValues(t *testing.T) {
	unsafeValue := "a:b=c|d"
	encodedValue := "base64%" + base64.StdEncoding.EncodeToString([]byte(unsafeValue))

	got := serializeOptions(map[string]string{
		"plain": "true",
		"note":  unsafeValue,
		"quiet": "false",
	})

	want := strings.Join([]string{
		"note=" + encodedValue,
		"plain=true",
		"quiet=false",
	}, ":")
	if got != want {
		t.Fatalf("unexpected serialized options:\nwant %q\ngot  %q", want, got)
	}

	options, ltx, err := DeserializeOptions(strings.Split(got, ":"))
	if err != nil {
		t.Fatalf("DeserializeOptions failed: %v", err)
	}
	if ltx != (lcontext.LContext{}) {
		t.Fatalf("unexpected lcontext: %#v", ltx)
	}
	if options["note"] != unsafeValue {
		t.Fatalf("expected note to round-trip, got %q", options["note"])
	}
	if options["plain"] != "true" {
		t.Fatalf("expected plain to round-trip, got %q", options["plain"])
	}
	if options["quiet"] != "false" {
		t.Fatalf("expected quiet to round-trip, got %q", options["quiet"])
	}
}

func TestDeserializeOptionsAcceptsRawSerializedBlob(t *testing.T) {
	unsafeValue := "a:b=c|d"
	serialized := serializeOptions(map[string]string{
		"note":  unsafeValue,
		"plain": "true",
		"quiet": "false",
	})

	options, ltx, err := DeserializeOptions([]string{serialized})
	if err != nil {
		t.Fatalf("DeserializeOptions failed: %v", err)
	}
	if ltx != (lcontext.LContext{}) {
		t.Fatalf("unexpected lcontext: %#v", ltx)
	}
	if options["note"] != unsafeValue {
		t.Fatalf("expected note to round-trip, got %q", options["note"])
	}
	if options["plain"] != "true" {
		t.Fatalf("expected plain to round-trip, got %q", options["plain"])
	}
	if options["quiet"] != "false" {
		t.Fatalf("expected quiet to round-trip, got %q", options["quiet"])
	}
}

func lcontextForTest(maxCount, before, after int) lcontext.LContext {
	return lcontext.LContext{
		MaxCount:      maxCount,
		BeforeContext: before,
		AfterContext:  after,
	}
}
