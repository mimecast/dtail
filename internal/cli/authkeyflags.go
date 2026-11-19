package cli

import (
	"flag"

	"github.com/mimecast/dtail/internal/config"
)

const authKeyPathHelpText = "Path to auth key/private key (defaults to ~/.ssh/id_rsa via config)"

// BindAuthKeyFlags registers the legacy and current auth-key flags.
func BindAuthKeyFlags(fs *flag.FlagSet, legacyKey *string, args *config.Args) {
	fs.StringVar(legacyKey, "key", "", "Deprecated alias for -auth-key-path")
	fs.StringVar(&args.SSHPrivateKeyFilePath, "auth-key-path", "", authKeyPathHelpText)
}

// FlagWasSet reports whether the named flag was explicitly set in fs.
func FlagWasSet(fs *flag.FlagSet, name string) bool {
	var wasSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

// ApplyAuthKeyPathCompatibility copies the deprecated legacy key into args when
// the new flag was not explicitly set. If both were set, the new flag wins and
// a warning message is returned.
func ApplyAuthKeyPathCompatibility(args *config.Args, legacyKey string, authKeyPathSet bool) string {
	if authKeyPathSet {
		if legacyKey != "" {
			return "WARN: -key is deprecated; ignoring it because -auth-key-path was also set"
		}
		return ""
	}

	if legacyKey != "" {
		args.SSHPrivateKeyFilePath = legacyKey
	}
	return ""
}
