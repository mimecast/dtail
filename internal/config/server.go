package config

import (
	"errors"
)

// Permissions map. Each SSH user has a list of permissions which
// log files it is allowed to follow and which ones not.
type Permissions struct {
	// The default user permissions.
	Default []string
	// The per user special permissions.
	Users map[string][]string
}

// Scheduled allows to configure scheduled mapreduce jobs.
type Scheduled struct {
	Name      string
	Enable    bool
	Files     string
	Query     string
	Outfile   string
	Discovery string   `json:",omitempty"`
	Servers   []string `json:",omitempty"`
	TimeRange [2]int
	AllowFrom []string `json:",omitempty"`
}

// ServerConfig represents the server configuration.
type ServerConfig struct {
	// The SSH server bind port.
	SSHBindAddress string
	// The max amount of concurrent user connection allowed to connect to the server.
	MaxConnections int
	// The max amount of concurrent cats per server.
	MaxConcurrentCats int
	// The max amount of concurrent tails per server.
	MaxConcurrentTails int
	// The user permissions.
	Permissions Permissions `json:",omitempty"`
	// The mapr log format
	MapreduceLogFormat string `json:",omitempty"`
	// The default path of the server host key
	HostKeyFile string
	// The host key size in bits
	HostKeyBits int
	// Scheduled mapreduce jobs.
	Schedule []Scheduled `json:",omitempty"`
}

// Create a new default server configuration.
func newDefaultServerConfig() *ServerConfig {
	defaultPermissions := []string{"^/.*"}
	defaultBindAddress := "0.0.0.0"

	return &ServerConfig{
		SSHBindAddress:     defaultBindAddress,
		MaxConnections:     10,
		MaxConcurrentCats:  2,
		MaxConcurrentTails: 50,
		HostKeyFile:        "./cache/ssh_host_key",
		HostKeyBits:        4096,
		Permissions: Permissions{
			Default: defaultPermissions,
		},
	}
}

// ServerUserPermissions retrieves the permission set of a given user.
func ServerUserPermissions(userName string) (permissions []string, err error) {
	permissions = Server.Permissions.Default
	if p, ok := Server.Permissions.Users[userName]; ok {
		permissions = p
	}

	if len(permissions) == 0 {
		err = errors.New("Empty set of permission, user won't be able to open any files")
	}

	return
}
