package source

type Source int

const (
	Client Source = iota
	Server Source = iota
)

func (s Source) String() string {
	switch s {
	case Client:
		return "CLIENT"
	case Server:
		return "SERVER"
	}

	panic("Unknown log source type")
}
