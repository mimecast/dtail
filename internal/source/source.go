package source

type Source int

const (
	Client      Source = iota
	Server      Source = iota
	HealthCheck Source = iota
)

func (s Source) String() string {
	switch s {
	case Client:
		return "CLIENT"
	case Server:
		return "SERVER"
	case HealthCheck:
		return "HEALTHCHECK"
	}

	panic("Unknown source type")
}
