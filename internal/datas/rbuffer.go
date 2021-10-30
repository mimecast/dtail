package datas

import "fmt"

// TODO: Unused code file, delete it.

// RBuffer is a simple circular string ring buffer data structure.
type RBuffer struct {
	Capacity int
	size     int
	readPos  int
	writePos int
	data     []string
}

// NewRBuffer creates a new string ring buffer.
func NewRBuffer(capacity int) (*RBuffer, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("RBuffer capacity must not be less than 1")
	}

	r := RBuffer{
		Capacity: capacity,
		size:     capacity + 1,
		data:     make([]string, capacity+1),
	}

	return &r, nil
}

// Add a value.
func (r *RBuffer) Add(value string) {
	r.data[r.writePos] = value
	r.writePos = (r.writePos + 1) % r.size

	if r.writePos == r.readPos {
		r.readPos = (r.readPos + 1) % r.size
	}
}

// Get a value.
func (r *RBuffer) Get() (string, bool) {
	if r.readPos == r.writePos {
		// RBuffer is empty.
		return "", false
	}

	value := r.data[r.readPos]
	r.readPos = (r.readPos + 1) % r.size
	return value, true
}
