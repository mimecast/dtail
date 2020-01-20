package fs

// Used to calculate how many log lines matched the regular expression
// and how many log files could be transmitted from the server to the client.
// Hit and transmit percentage takes only the last 100 log lines into calculation.
type stats struct {
	pos           int
	lineCount     uint64
	matched       [100]bool
	matchCount    uint64
	transmitted   [100]bool
	transmitCount int
}

// Return the total line count.
func (f *stats) totalLineCount() uint64 {
	return f.lineCount
}

// Calculate the percentage of log lines transmitted to the client.
func (f *stats) transmittedPerc() int {
	return int(percentOf(float64(f.matchCount), float64(f.transmitCount)))
}

// Update bucket position. We only take into consideration the last 100
// lines for stats.
func (f *stats) updatePosition() {
	f.pos = (f.pos + 1) % 100
	f.lineCount++
}

// Increment match counter.
func (f *stats) updateLineMatched() {
	if !f.matched[f.pos] {
		f.matchCount++
		f.matched[f.pos] = true
	}
}

// Increment transmitted counter.
func (f *stats) updateLineTransmitted() {
	if !f.transmitted[f.pos] {
		f.transmitCount++
		f.transmitted[f.pos] = true
	}
}

// Decrement match counter.
func (f *stats) updateLineNotMatched() {
	if f.matched[f.pos] {
		f.matchCount--
		f.matched[f.pos] = false
	}
}

// Decrement transmitted counter.
func (f *stats) updateLineNotTransmitted() {
	if f.transmitted[f.pos] {
		f.transmitCount--
		f.transmitted[f.pos] = false
	}
}

func percentOf(total float64, value float64) float64 {
	if total == 0 || total == value {
		return 100
	}
	return value / (total / 100.0)
}
