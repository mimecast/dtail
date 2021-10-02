package source

type Source int

const (
	Client Source = iota
	Server Source = iota
)

func (s Source) String() string {
	switch s {
	case Client:
		return "Client"
	case Server:
		return "Server"
	}

	panic("Unknown log source type")
}
