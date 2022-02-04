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
