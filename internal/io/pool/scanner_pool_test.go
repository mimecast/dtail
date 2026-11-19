package pool

import "testing"

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

func TestSmallBufferPoolClearsContentsOnPut(t *testing.T) {
	buffer := []byte{1, 2, 3, 4}
	PutSmallBuffer(&buffer)
	for i, value := range buffer {
		if value != 0 {
			t.Fatalf("buffer[%d] = %d, want 0", i, value)
		}
	}
}
