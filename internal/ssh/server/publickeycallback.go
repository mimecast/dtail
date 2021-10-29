package server

import (
	"fmt"
	"io/ioutil"
	"os"
	goUser "os/user"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	user "github.com/mimecast/dtail/internal/user/server"

	gossh "golang.org/x/crypto/ssh"
)

// PublicKeyCallback is for the server to check whether a public SSH key is
// authorized ot not.
func PublicKeyCallback(c gossh.ConnMetadata,
	offeredPubKey gossh.PublicKey) (*gossh.Permissions, error) {

	user, err := user.New(c.User(), c.RemoteAddr().String())
	if err != nil {
		return nil, err
	}

	dlog.Common.Info(user, "Incoming authorization")
	if config.ServerRelaxedAuthEnable {
		dlog.Common.Fatal(user, "Granting permissions via relaxed-auth")
		return nil, nil
	}

	authorizedKeysFile, err := authorizedKeysFile(user)
	if err != nil {
		return nil, err
	}

	dlog.Common.Info(user, "Reading", authorizedKeysFile)
	authorizedKeysBytes, err := ioutil.ReadFile(authorizedKeysFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to read authorized keys file|%s|%s|%s",
			authorizedKeysFile, user, err.Error())
	}

	return verifyAuthorizedKeys(user, authorizedKeysBytes, offeredPubKey)
}

func verifyAuthorizedKeys(user *user.User, authorizedKeysBytes []byte,
	offeredPubKey gossh.PublicKey) (*gossh.Permissions, error) {

	authorizedKeysMap := map[string]bool{}
	for len(authorizedKeysBytes) > 0 {
		authorizedPubKey, _, _, restBytes, err := gossh.ParseAuthorizedKey(authorizedKeysBytes)
		if err != nil {
			return nil, fmt.Errorf("unable to parse authorized keys bytes|%s|%s",
				user, err.Error())
		}
		authorizedKeysMap[string(authorizedPubKey.Marshal())] = true
		authorizedKeysBytes = restBytes
		dlog.Common.Debug(user, "Authorized public key fingerprint",
			gossh.FingerprintSHA256(authorizedPubKey))
	}

	dlog.Common.Debug(user, "Offered public key fingerprint", gossh.FingerprintSHA256(offeredPubKey))
	if authorizedKeysMap[string(offeredPubKey.Marshal())] {
		return &gossh.Permissions{
			Extensions: map[string]string{"pubkey-fp": gossh.FingerprintSHA256(offeredPubKey)},
		}, nil
	}

	return nil, fmt.Errorf("%s|public key of user not authorized", user)
}

func authorizedKeysFile(user *user.User) (string, error) {
	if config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		// In this case, we expect a pub key in the current directory.
		return "./id_rsa.pub", nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Check for cached version in the dserver directory.
	authorizedKeysFile := fmt.Sprintf("%s/%s/%s.authorized_keys", cwd,
		config.Common.CacheDir, user.Name)
	if _, err = os.Stat(authorizedKeysFile); err == nil {
		return authorizedKeysFile, nil
	}

	// As the last option, check the regular SSH path.
	osUser, err := goUser.Lookup(user.Name)
	if err != nil {
		return "", err
	}
	authorizedKeysFile = fmt.Sprintf("%s/.ssh/authorized_keys", osUser.HomeDir)
	if _, err = os.Stat(authorizedKeysFile); err == nil {
		return authorizedKeysFile, nil
	}

	return "", fmt.Errorf("unable to find a any authorized keys file")
}
