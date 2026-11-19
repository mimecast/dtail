package config

import (
	"errors"
)

const defaultServerHostKeyPath = "./cache/ssh_host_key"

// Permissions map. Each SSH user has a list of permissions which log files it
// is allowed to follow and which ones not.
type Permissions struct {
	// The default user permissions.
	Default []string
	// The per user special permissions.
	Users map[string][]string
}

// JobCommons summarises common job fields
type jobCommons struct {
	Name      string
	Enable    bool
	Files     string
	Query     string
	Outfile   string   `json:",omitempty"`
	Discovery string   `json:",omitempty"`
	Servers   []string `json:",omitempty"`
	AllowFrom []string `json:",omitempty"`
}

// Scheduled allows to configure scheduled mapreduce jobs.
type Scheduled struct {
	jobCommons
	TimeRange [2]int
}

// Continuous allows to configure continuous running mapreduce jobs.
type Continuous struct {
	jobCommons
	RestartOnDayChange bool `json:",omitempty"`
}

// ServerConfig represents the server configuration.
type ServerConfig struct {
	// The SSH server bind port.
	SSHBindAddress string
	// The max amount of concurrent user connection allowed to connect to the server.
	MaxConnections int
	// Rolling inactivity timeout for authenticated SSH sessions, in seconds.
	// The close lags this value by two to three refresh intervals (see
	// DefaultIdleSessionTimeoutS for that window and for the one second
	// interval floor, which makes timeouts below four seconds close two to
	// three whole seconds late); omitting the key or setting it to 0 or less
	// uses the DefaultIdleSessionTimeoutS 900 second default.
	IdleSessionTimeoutS int `json:",omitempty"`
	// The max amount of concurrent cats per server.
	MaxConcurrentCats int
	// The max amount of concurrent tails per server.
	MaxConcurrentTails int
	// The max line length until it's split up into multiple smaller lines.
	MaxLineLength int
	// The user permissions.
	Permissions Permissions `json:",omitempty"`
	// The mapr log format
	MapreduceLogFormat string `json:",omitempty"`
	// HostKeyFile is the legacy private SSH host-key path.
	//
	// Deprecated: use HostKeyPath. Existing configuration files remain supported.
	HostKeyFile string
	// HostKeyPath selects the private SSH host key. When empty, HostKeyFile is
	// used for compatibility; when both are empty, the built-in path is used.
	HostKeyPath string `json:",omitempty"`
	// AuthorizedKeysPath selects one authorized_keys file for all users. When
	// empty, the per-user cache and ~/.ssh/authorized_keys lookup is used.
	AuthorizedKeysPath string `json:",omitempty"`
	// The host key size in bits
	HostKeyBits int
	// Scheduled mapreduce jobs.
	Schedule []Scheduled `json:",omitempty"`
	// Continuous mapreduce jobs
	Continuous []Continuous `json:",omitempty"`
	// The allowed key exchanges algorithms.
	KeyExchanges []string `json:",omitempty"`
	// The allowed cipher algorithms.
	Ciphers []string `json:",omitempty"`
	// The allowed MAC algorithms.
	MACs []string `json:",omitempty"`
	// Enable in-memory auth-key registration and fast reconnect.
	AuthKeyEnabled bool `json:",omitempty"`
	// Auth-key cache entry TTL in seconds.
	AuthKeyTTLSeconds int `json:",omitempty"`
	// Maximum number of cached auth keys per user.
	AuthKeyMaxPerUser int `json:",omitempty"`
	// Retry interval for glob retries in milliseconds.
	ReadGlobRetryIntervalMs int `json:",omitempty"`
	// Retry interval for re-reading in tail/cat loops in milliseconds.
	ReadRetryIntervalMs int `json:",omitempty"`
	// Maximum payload backing memory retained by the output buffer per SSH session.
	OutputBufferMaxBytes int `json:",omitempty"`
	// Maximum time to wait for the session reader to drain output, in milliseconds.
	OutputFlushTimeoutMs int `json:",omitempty"`
	// Compatibility fallback interval for detecting a stale generation when an
	// older output writer provides no cancellation signal. Values below the
	// implementation safety minimum are clamped.
	OutputReadRetryIntervalMs int `json:",omitempty"`
	// Maximum time to wait for output EOF acknowledgement after signaling EOF, in milliseconds.
	OutputEOFAckTimeoutMs int `json:",omitempty"`
	// Maximum size in bytes of a single command frame (bytes accumulated between
	// ';' delimiters). Frames that grow beyond this limit are rejected and the
	// session is closed to prevent unbounded memory exhaustion by a malicious or
	// misbehaving client. Default is 1 MiB. 0 or less disables the limit for
	// normal sessions; health-check sessions then keep the 1 MiB default.
	MaxCommandFrameSize int `json:",omitempty"`
	// Maximum number of glob expansion targets (file paths) that a single read
	// command is allowed to dispatch. When a glob pattern expands to more paths
	// than this limit, the excess paths are dropped and a warning is sent to the
	// client. This prevents an authenticated user with broad read permission from
	// spawning unbounded goroutines and exhausting server memory/CPU.
	// Default is 1000. Set to 0 to keep the built-in default.
	MaxGlobTargets int `json:",omitempty"`
}

// Create a new default server configuration.
func newDefaultServerConfig() *ServerConfig {
	defaultPermissions := []string{"^/.*"}
	defaultBindAddress := "0.0.0.0"
	return &ServerConfig{
		HostKeyBits:         4096,
		MapreduceLogFormat:  "default",
		MaxConcurrentCats:   2,
		MaxConcurrentTails:  50,
		MaxConnections:      10,
		IdleSessionTimeoutS: DefaultIdleSessionTimeoutS,
		MaxLineLength:       1024 * 1024,
		SSHBindAddress:      defaultBindAddress,
		Permissions: Permissions{
			Default: defaultPermissions,
		},
		AuthKeyEnabled:            true,
		AuthKeyTTLSeconds:         86400,
		AuthKeyMaxPerUser:         5,
		ReadGlobRetryIntervalMs:   5000,
		ReadRetryIntervalMs:       2000,
		OutputBufferMaxBytes:      DefaultOutputBufferMaxBytes,
		OutputFlushTimeoutMs:      2000,
		OutputReadRetryIntervalMs: 1,
		OutputEOFAckTimeoutMs:     2000,
		MaxCommandFrameSize:       DefaultMaxCommandFrameSize,
		MaxGlobTargets:            1000,
	}
}

// NewDefaultServerConfigForTest returns a fresh ServerConfig populated with all
// default values. It is intended for use in unit tests that need to inspect or
// compare default configuration without running the full config initializer.
func NewDefaultServerConfigForTest() *ServerConfig {
	return newDefaultServerConfig()
}

// EffectiveHostKeyPath returns the configured SSH host-key path, including
// compatibility with the legacy HostKeyFile setting.
func (c *ServerConfig) EffectiveHostKeyPath() string {
	if c == nil {
		return ""
	}
	if c.HostKeyPath != "" {
		return c.HostKeyPath
	}
	if c.HostKeyFile != "" {
		return c.HostKeyFile
	}
	return defaultServerHostKeyPath
}

// UserPermissions retrieves the permission set of a given user.
func (c *ServerConfig) UserPermissions(userName string) (permissions []string, err error) {
	if c == nil {
		return nil, errors.New("missing server config")
	}

	permissions = c.Permissions.Default
	if p, ok := c.Permissions.Users[userName]; ok {
		permissions = p
	}
	if len(permissions) == 0 {
		err = errors.New("empty set of permission, user won't be able to open any files")
	}
	return
}

// ServerUserPermissions retrieves the permission set of a given user.
func ServerUserPermissions(userName string) (permissions []string, err error) {
	if Server == nil {
		return nil, errors.New("missing server config")
	}
	return Server.UserPermissions(userName)
}
