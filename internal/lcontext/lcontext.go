package lcontext

// LContext stands for line context and is here to help filtering out only specific lines.
type LContext struct {
	AfterContext  int
	BeforeContext int
	MaxCount      int
}
