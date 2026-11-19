package pool

import (
	"bytes"
	"testing"
)

func TestLargeBufferPoolsPreserveContentsOnPut(t *testing.T) {
	tests := []struct {
		name string
		put  func(*[]byte)
	}{
		{name: "scanner", put: PutScannerBuffer},
		{name: "medium", put: PutMediumBuffer},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := []byte{1, 2, 3, 4}
			test.put(&buffer)
			if got := buffer; got[0] != 1 || got[1] != 2 || got[2] != 3 || got[3] != 4 {
				t.Fatalf("Put changed buffer contents: %v", got)
			}
		})
	}
}

func TestSmallBufferPoolRestoresLengthWithoutClearingContents(t *testing.T) {
	buffer := []byte{1, 2, 3, 4}
	buffer = buffer[:0]
	PutSmallBuffer(&buffer)

	if got, want := len(buffer), cap(buffer); got != want {
		t.Fatalf("buffer length = %d, want restored capacity %d", got, want)
	}
	if got, want := buffer, []byte{1, 2, 3, 4}; !bytes.Equal(got, want) {
		t.Fatalf("PutSmallBuffer changed contents: %v, want %v", got, want)
	}
}

func TestPrepareSmallBufferRejectsNilPointer(t *testing.T) {
	if prepareSmallBuffer(nil) {
		t.Fatal("prepareSmallBuffer accepted a nil pointer")
	}
}
