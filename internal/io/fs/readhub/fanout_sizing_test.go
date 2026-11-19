package readhub

import (
	"bytes"
	"testing"
)

func TestFanoutSparseCapacityAndDenseBoundary(t *testing.T) {
	p, sub := fanoutUnderTest(t)
	raw := bytes.Repeat([]byte("x"), 128)
	feed := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := p.ProcessRawLine(raw, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	flush := func() *chunk {
		t.Helper()
		if err := p.Flush(); err != nil {
			t.Fatal(err)
		}
		items := drain(sub)
		if len(items) != 1 || items[0].kind != chunkItem {
			t.Fatalf("want exactly one chunk, got %v", items)
		}
		return items[0].chunk
	}
	feed(1)
	sparse := flush()
	if cap(sparse.data) > 4096 || cap(sparse.ends) > 8 || cap(sparse.offsets) > 8 {
		t.Fatalf("sparse capacities: %d %d %d", cap(sparse.data), cap(sparse.ends), cap(sparse.offsets))
	}
	feed(512)
	if items := drain(sub); len(items) != 0 {
		t.Fatal("growing beyond the initial reservation published prematurely")
	}
	dense := flush()
	if len(dense.data) != 65536 || len(dense.ends) != 512 || cap(dense.data) != 65536 {
		t.Fatalf("dense chunk: bytes=%d lines=%d cap=%d", len(dense.data), len(dense.ends), cap(dense.data))
	}
	// The first smaller read may reserve the previous metadata size, but
	// never its payload size. The next must forget the metadata reservation
	// too, without modifying either of the still-held publications.
	feed(1)
	if c := flush(); cap(c.data) > 4096 {
		t.Fatal("sparse read reserved the preceding dense payload size")
	}
	feed(1)
	smallAgain := flush()
	if cap(smallAgain.data) > 4096 || cap(smallAgain.ends) > 8 {
		t.Fatal("dense read reservation stuck after sparse reads")
	}
	if !bytes.Equal(sparse.data, raw) || !bytes.Equal(dense.data, bytes.Repeat(raw, 512)) {
		t.Fatal("published payload was modified by a later read")
	}
}

func TestFanoutOversizedAndEmptyLinesKeepPublicationOrder(t *testing.T) {
	p, sub := fanoutUnderTest(t)
	large := bytes.Repeat([]byte("L"), 2*chunkSize)
	for _, raw := range [][]byte{large, nil, []byte("next")} {
		if err := p.ProcessRawLine(raw, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	items := drain(sub)
	if len(items) != 2 || len(items[0].chunk.ends) != 2 {
		t.Fatalf("oversized and empty lines should share their publication: %v", items)
	}
	if !bytes.Equal(items[0].chunk.line(0), large) || len(items[0].chunk.line(1)) != 0 || string(items[1].chunk.line(0)) != "next" {
		t.Fatal("wrong line contents/order")
	}
}

func TestFanoutMetadataHintIsBoundedAfterShortLines(t *testing.T) {
	p, sub := fanoutUnderTest(t)
	for i := 0; i < 8192; i++ {
		if err := p.ProcessRawLine([]byte("\n"), 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	first := drain(sub)
	if len(first) != 1 || len(first[0].chunk.ends) != 8192 {
		t.Fatal("short-line chunk lost lines or published early")
	}
	if err := p.ProcessRawLine([]byte("later"), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	second := drain(sub)
	if len(second) != 1 {
		t.Fatal("next read was not published")
	}
	c := second[0].chunk
	if cap(c.ends) > 512 || cap(c.offsets) > 512 || cap(c.data) > 4096 {
		t.Fatalf("outlier reservation persisted: %d %d %d", cap(c.ends), cap(c.offsets), cap(c.data))
	}
	if !bytes.Equal(first[0].chunk.data, bytes.Repeat([]byte("\n"), 8192)) {
		t.Fatal("earlier publication mutated")
	}
}
