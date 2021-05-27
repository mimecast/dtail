package datas

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestRBufferOneElement(t *testing.T) {
	r, err := NewRBuffer(1)
	if err != nil {
		t.Errorf("Expected error creating ring buffer with capacity 1")
	}

	testRBufferValues(t, r, []string{"Hello world"})
	testRBufferValues(t, r, []string{"Hello world", "Hello universe"})
}

func TestRBuffer(t *testing.T) {
	if _, err := NewRBuffer(0); err == nil {
		t.Errorf("Expected error creating ring buffer with capacity 0")
	}

	r, err := NewRBuffer(10)
	if err != nil {
		t.Errorf("Error creating ring buffer with capacity 10: %v", err)
	}

	fiveValues := []string{
		"42 is the answer!",
		"Scorpion: Get over here!",
		"Have you swiped your nectar card?",
		"Please mind the gap between the train and the platform!",
		"Visit DTail at https://dtail.dev",
	}
	testRBufferValues(t, r, fiveValues)

	moreFiveValues := []string{
		"I love Golang",
		"As a contrast, I also love Perl",
		"Mimecast: Stop Bad Things From Happening to Good Organizations",
		"We are the Buetow Brothers",
		"London is calling",
	}
	tenValues := append(fiveValues, moreFiveValues...)
	testRBufferValues(t, r, tenValues)
}

func TestRandomRBuffer(t *testing.T) {
	for i := 0; i < 100; i++ {
		testRandomRBuffer(t)
	}
}

func testRandomRBuffer(t *testing.T) {
	rand.Seed(time.Now().UnixNano())

	maxCapacity := 1000
	minCapacity := 1
	capacity := rand.Intn(maxCapacity-minCapacity) + minCapacity
	r, err := NewRBuffer(capacity)
	if err != nil {
		t.Errorf("Error creating ring buffer with capacity %d: %v", capacity, err)
	}

	numValues := rand.Intn(capacity * 2)
	values := make([]string, numValues)
	for i := 0; i < numValues; i++ {
		values = append(values, fmt.Sprintf("%d.%d", i, rand.Int()))
	}

	testRBufferValues(t, r, values)
}

func testRBufferValues(t *testing.T, r *RBuffer, values []string) {
	value, ok := r.Get()
	if ok {
		t.Errorf("Expected not ok reading from empty ring buffer but got ok and  value '%s'", value)
	}

	for _, value := range values {
		r.Add(value)
	}

	expectedValues := values
	overCapacity := len(values) - r.Capacity
	if overCapacity > 0 {
		expectedValues = values[overCapacity:]
	}

	for _, expected := range expectedValues {
		value, ok := r.Get()
		if !ok {
			t.Errorf("Expected value '%s' but got nothing", expected)
		}
		if value != expected {
			t.Errorf("Expected value '%s' but got value '%v'", expected, value)
		}
	}

	value, ok = r.Get()
	if ok {
		t.Errorf("Expected not ok reading from empty ring buffer but got ok and  value '%s'", value)
	}
}
