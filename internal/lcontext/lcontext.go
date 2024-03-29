package lcontext

// LContext stands for line context (used by context aware grep queries e.g.)
type LContext struct {
	AfterContext  int
	BeforeContext int
	MaxCount      int
}

// Has returns true if it has any parameter set.
func (c LContext) Has() bool {
	if c.AfterContext > 0 {
		return true
	}
	if c.BeforeContext > 0 {
		return true
	}
	if c.MaxCount > 0 {
		return true
	}
	return false
}
