package config

// ClientColorConfig allows to override the default terminal color codes.
type ClientColorConfig struct {
	OkBgColor Color
}

/*
	splitted := strings.Split(line, "|")
	if splitted[2] == "100" {
		splitted[2] = Paint(BgGreen, splitted[2])
	} else {
		splitted[2] = Paint(BgRed, splitted[2])
	}
	info := strings.Join(splitted[0:5], "|")
	log := strings.Join(splitted[5:], "|")
*/

// ClientConfig represents a DTail client configuration (empty as of now as there
// are no available config options yet, but that may changes in the future).
type ClientConfig struct {
	TerminalColors ClientColorConfig `json:",omitempty"`
}

// Create a new default client configuration.
func newDefaultClientConfig() *ClientConfig {
	return &ClientConfig{}
}
