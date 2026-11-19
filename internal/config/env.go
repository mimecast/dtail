package config

import "os"

// Env returns true when a given environment variable is set to "yes".
func Env(env string) bool {
	return os.Getenv(env) == "yes"
}

// IntegrationSSHPrivateKeyPath returns the bootstrap key path used by the
// legacy integration harness. Runtime SSH setup receives the resolved path
// through Args; this helper remains for integration-test compatibility.
func IntegrationSSHPrivateKeyPath() string {
	return integrationSSHPrivateKeyPath(os.Getenv("DTAIL_AUTH_KEY_PATH"))
}

// Hostname returns the configured hostname override or the system hostname.
// Environment compatibility is resolved once by Setup rather than here.
func Hostname() (string, error) {
	return CurrentRuntime().Hostname()
}
