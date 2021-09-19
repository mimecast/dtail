package dlog

type source int

const (
	CLIENT source = iota
	SERVER source = iota
)

func (s source) String() string {
	switch s {
	case CLIENT:
		return "CLIENT"
	case SERVER:
		return "SERVER"
	}

	panic("Unknown log source type")
}
