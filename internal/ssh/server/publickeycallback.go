package server

import (
	"bytes"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	goUser "os/user"
	"path/filepath"
	"strings"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
	user "github.com/mimecast/dtail/internal/sessionuser"

	gossh "golang.org/x/crypto/ssh"
)

type authorizedKeyParser func([]byte) (gossh.PublicKey, string, []string, []byte, error)

// NewPublicKeyCallback creates an instance-scoped SSH public key callback.
// keyStore must be non-nil; callers are responsible for constructing and
// wiring the store. There is no shared package-level fallback.
func NewPublicKeyCallback(authKeyEnabled bool, cacheDir, authorizedKeysPath string,
	keyStore *authkey.Store, logger logging.Logger) func(gossh.ConnMetadata, gossh.PublicKey) (*gossh.Permissions, error) {

	if keyStore == nil {
		panic("NewPublicKeyCallback: keyStore must not be nil")
	}
	logger = logging.OrNop(logger)
	return func(c gossh.ConnMetadata, offeredPubKey gossh.PublicKey) (*gossh.Permissions, error) {
		return publicKeyCallback(c, offeredPubKey, authKeyEnabled, cacheDir, authorizedKeysPath, keyStore, logger)
	}
}

func publicKeyCallback(c gossh.ConnMetadata, offeredPubKey gossh.PublicKey,
	authKeyEnabled bool, cacheDir, authorizedKeysPath string, keyStore *authkey.Store,
	logger logging.Logger) (*gossh.Permissions, error) {

	// The user name becomes part of the authorized keys cache path
	// (<CacheDir>/<name>.authorized_keys). A name such as "../../tmp/x" would
	// make dserver trust a key file outside the cache directory, and such a
	// user has no entry in Permissions.Users, so it would get the default
	// permissions.
	if !validUserName(c.User()) {
		return nil, fmt.Errorf("invalid user name %q", c.User())
	}

	if config.IsPasswordOnlyUser(c.User()) {
		return nil, fmt.Errorf("user %s does not support public key authentication", c.User())
	}

	user, err := user.New(c.User(), c.RemoteAddr().String(), nil, logger)
	if err != nil {
		return nil, err
	}
	logger.Info(user, "Incoming authorization")

	if authKeyEnabled {
		if permissions := authKeyStorePermissions(keyStore, user.Name, offeredPubKey); permissions != nil {
			logger.Info(user, "Authorized by in-memory auth key store")
			return permissions, nil
		}
	}

	rootedAuthorizedKeysPath, err := authorizedKeysPathForUser(user, cacheDir, authorizedKeysPath)
	if err != nil {
		return nil, err
	}

	logger.Info(user, "Reading", rootedAuthorizedKeysPath.Path())
	authorizedKeysBytes, err := rootedAuthorizedKeysPath.ReadFile()
	if err != nil {
		return nil, fmt.Errorf("unable to read authorized keys file|%s|%s|%s",
			rootedAuthorizedKeysPath.Path(), user, err.Error())
	}

	return verifyAuthorizedKeys(user, authorizedKeysBytes, offeredPubKey, logger)
}

// validUserName reports whether name can be used as a single path element.
func validUserName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

func verifyAuthorizedKeys(user *user.User, authorizedKeysBytes []byte,
	offeredPubKey gossh.PublicKey, logger logging.Logger) (*gossh.Permissions, error) {
	return verifyAuthorizedKeysWithParser(user, authorizedKeysBytes, offeredPubKey, gossh.ParseAuthorizedKey, logger)
}

func verifyAuthorizedKeysWithParser(user *user.User, authorizedKeysBytes []byte,
	offeredPubKey gossh.PublicKey, parseAuthorizedKey authorizedKeyParser,
	logger logging.Logger) (*gossh.Permissions, error) {

	// Parse line by line. Handing the parser all remaining bytes made it skip
	// to the next key itself and fail only at the end of the input, after
	// which the rest was parsed again from the next line on: every trailing
	// comment or blank line then cost one warning and one more pass over the
	// rest (quadratic), before authentication and once per offered key.
	authorizedKeysMap := map[string]bool{}
	for len(authorizedKeysBytes) > 0 {
		line := authorizedKeysBytes
		if lineEnd := bytes.IndexByte(authorizedKeysBytes, '\n'); lineEnd >= 0 {
			line = authorizedKeysBytes[:lineEnd+1]
		}
		authorizedKeysBytes = authorizedKeysBytes[len(line):]
		if !hasAuthorizedKeysContent(line) {
			continue
		}
		authorizedPubKey, _, _, _, err := parseAuthorizedKey(line)
		if err != nil {
			logger.Warn(user, "Skipping unparseable authorized_keys line", err)
			continue
		}
		authorizedKeysMap[string(authorizedPubKey.Marshal())] = true
		logger.Debug(user, "Authorized public key fingerprint",
			gossh.FingerprintSHA256(authorizedPubKey))
	}

	logger.Debug(user, "Offered public key fingerprint", gossh.FingerprintSHA256(offeredPubKey))
	if authorizedKeysMap[string(offeredPubKey.Marshal())] {
		return permissionsFromPublicKey(offeredPubKey), nil
	}

	return nil, fmt.Errorf("%s|public key of user not authorized", user)
}

// hasAuthorizedKeysContent reports whether data has a line that is neither
// blank nor a comment, i.e. one to read as a key.
func hasAuthorizedKeysContent(data []byte) bool {
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] != '#' {
			return true
		}
	}
	return false
}

func authKeyStorePermissions(keyStore *authkey.Store, userName string,
	offeredPubKey gossh.PublicKey) *gossh.Permissions {

	if keyStore == nil || !keyStore.Has(userName, offeredPubKey) {
		return nil
	}

	return permissionsFromPublicKey(offeredPubKey)
}

func permissionsFromPublicKey(offeredPubKey gossh.PublicKey) *gossh.Permissions {
	return &gossh.Permissions{
		Extensions: map[string]string{"pubkey-fp": gossh.FingerprintSHA256(offeredPubKey)},
	}
}

type userLookupFunc func(string) (*goUser.User, error)

func authorizedKeysPathForUser(user *user.User, cacheDir, configuredPath string) (fs.RootedPath, error) {
	if configuredPath != "" {
		return fs.NewRootedPath(configuredPath)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fs.RootedPath{}, err
	}

	return findAuthorizedKeysPath(user, cacheDir, cwd, goUser.Lookup)
}

func findAuthorizedKeysPath(user *user.User, cacheDir, cwd string,
	lookupUser userLookupFunc) (fs.RootedPath, error) {

	// Check for cached version in the dserver directory. An absolute
	// CacheDir (as used by the BSD packages, e.g. /var/run/dserver/cache)
	// must be used as-is: joining it with the CWD would break auth
	// whenever dserver is started from a directory other than / (e.g. a
	// manual rc.d restart from a home directory). Relative CacheDirs stay
	// relative to the CWD as before.
	if cacheDir != "" {
		cacheBase := cacheDir
		if !filepath.IsAbs(cacheDir) {
			cacheBase = filepath.Join(cwd, cacheDir)
		}
		cachePath := filepath.Join(cacheBase, fmt.Sprintf("%s.authorized_keys", user.Name))
		rootedCachePath, err := fs.NewRootedPath(cachePath)
		if err != nil {
			return fs.RootedPath{}, err
		}
		if _, err := rootedCachePath.Stat(); err == nil {
			return rootedCachePath, nil
		}
	}

	// As the last option, check the regular SSH path.
	osUser, err := lookupUser(user.Name)
	if err != nil {
		return fs.RootedPath{}, err
	}
	authorizedKeysPath := filepath.Join(osUser.HomeDir, ".ssh", "authorized_keys")
	rootedAuthorizedKeysPath, err := fs.NewRootedPath(authorizedKeysPath)
	if err != nil {
		return fs.RootedPath{}, err
	}
	if _, err = rootedAuthorizedKeysPath.Stat(); err == nil {
		return rootedAuthorizedKeysPath, nil
	}
	if !errors.Is(err, iofs.ErrNotExist) {
		return fs.RootedPath{}, err
	}

	return fs.RootedPath{}, fmt.Errorf("unable to find any authorized keys file")
}
