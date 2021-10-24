package config

import "os"

// Env returns true when a given environment variable is set to "yes".
func Env(env string) bool {
	return "yes" == os.Getenv(env)
}

// Hostname returns the current hostname. It can be overriden with
// DTAIL_HOSTNAME_OVERRIDE environment variable (useful for integration tests).
func Hostname() (string, error) {
	hostname := os.Getenv("DTAIL_HOSTNAME_OVERRIDE")
	if len(hostname) > 0 {
		return hostname, nil
	}
	return os.Hostname()
}

// SSHKnownHostsFile returns the known hosts file path (useful for integration tests)
func SSHKnownHostsFile() string {
	if len(os.Getenv("DTAIL_SSH_KNOWN_HOSTS_FILE")) > 0 {
		return os.Getenv("DTAIL_SSH_KNOWN_HOSTS_FILE")
	} else {
		return os.Getenv("HOME") + "/.ssh/known_hosts"
	}
}
