package config

// CommonConfig stores configuration keys shared by DTail server and client.
type CommonConfig struct {
	SSHPort                    int
	ExperimentalFeaturesEnable bool `json:",omitempty"`
	DebugEnable                bool `json:",omitempty"`
	TraceEnable                bool `json:",omitempty"`
	// The log strategy to use, one of
	//   stdout: only log to stdout (useful when used with systemd)
	//   daily: create a log file for every day
	LogStrategy      string
	LogDir           string
	CacheDir         string
	TmpDir           string `json:",omitempty"`
	PProfEnable      bool   `json:",omitempty"`
	PProfPort        int    `json:",omitempty"`
	PProfBindAddress string `json:",omitempty"`
}

// Create a new default configuration.
func newDefaultCommonConfig() *CommonConfig {
	return &CommonConfig{
		SSHPort:                    2222,
		DebugEnable:                false,
		TraceEnable:                false,
		ExperimentalFeaturesEnable: false,
		LogDir:                     "log",
		CacheDir:                   "cache",
		TmpDir:                     "/tmp",
		PProfEnable:                false,
		PProfPort:                  6060,
		PProfBindAddress:           "0.0.0.0",
	}
}
