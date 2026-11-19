package server

import (
	"bytes"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	goUser "os/user"
	"path/filepath"

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

func verifyAuthorizedKeys(user *user.User, authorizedKeysBytes []byte,
	offeredPubKey gossh.PublicKey, logger logging.Logger) (*gossh.Permissions, error) {
	return verifyAuthorizedKeysWithParser(user, authorizedKeysBytes, offeredPubKey, gossh.ParseAuthorizedKey, logger)
}

func verifyAuthorizedKeysWithParser(user *user.User, authorizedKeysBytes []byte,
	offeredPubKey gossh.PublicKey, parseAuthorizedKey authorizedKeyParser,
	logger logging.Logger) (*gossh.Permissions, error) {

	authorizedKeysMap := map[string]bool{}
	for len(authorizedKeysBytes) > 0 {
		authorizedPubKey, _, _, restBytes, err := parseAuthorizedKey(authorizedKeysBytes)
		if err != nil {
			logger.Warn(user, "Skipping unparseable authorized_keys line", err)
			nextAuthorizedKeysBytes, ok := advanceToNextAuthorizedKeysLine(authorizedKeysBytes)
			if !ok {
				break
			}
			authorizedKeysBytes = nextAuthorizedKeysBytes
			continue
		}
		authorizedKeysMap[string(authorizedPubKey.Marshal())] = true
		authorizedKeysBytes = restBytes
		logger.Debug(user, "Authorized public key fingerprint",
			gossh.FingerprintSHA256(authorizedPubKey))
	}

	logger.Debug(user, "Offered public key fingerprint", gossh.FingerprintSHA256(offeredPubKey))
	if authorizedKeysMap[string(offeredPubKey.Marshal())] {
		return permissionsFromPublicKey(offeredPubKey), nil
	}

	return nil, fmt.Errorf("%s|public key of user not authorized", user)
}

func advanceToNextAuthorizedKeysLine(authorizedKeysBytes []byte) ([]byte, bool) {
	lineEnd := bytes.IndexByte(authorizedKeysBytes, '\n')
	if lineEnd == -1 {
		return nil, false
	}

	nextBytes := authorizedKeysBytes[lineEnd+1:]
	return nextBytes, true
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
