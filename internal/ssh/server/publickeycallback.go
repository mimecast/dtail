package server

import (
	"fmt"
	"io/ioutil"
	"os"
	osUser "os/user"

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
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("Unable to get current working directory|%s|", err.Error())
	}
	if config.ServerRelaxedAuthEnable {
		dlog.Common.Fatal(user, "Granting permissions via relaxed-auth")
		return nil, nil
	}

	authorizedKeysFile := fmt.Sprintf("%s/%s/%s.authorized_keys", cwd,
		config.Common.CacheDir, user.Name)
	if _, err := os.Stat(authorizedKeysFile); os.IsNotExist(err) {
		user, err := osUser.Lookup(user.Name)
		if err != nil {
			return nil, fmt.Errorf("Unable to authorize|%s|%s|", user, err.Error())
		}
		// Fallback to ~
		authorizedKeysFile = user.HomeDir + "/.ssh/authorized_keys"
	}

	dlog.Common.Info(user, "Reading", authorizedKeysFile)
	authorizedKeysBytes, err := ioutil.ReadFile(authorizedKeysFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to read authorized keys file|%s|%s|%s",
			authorizedKeysFile, user, err.Error())
	}

	authorizedKeysMap := map[string]bool{}
	for len(authorizedKeysBytes) > 0 {
		authorizedPubKey, _, _, restBytes, err := gossh.ParseAuthorizedKey(authorizedKeysBytes)
		if err != nil {
			return nil, fmt.Errorf("Unable to parse authorized keys bytes|%s|%s",
				user, err.Error())
		}
		authorizedKeysMap[string(authorizedPubKey.Marshal())] = true
		authorizedKeysBytes = restBytes
		dlog.Common.Debug(user, "Authorized public key fingerprint",
			gossh.FingerprintSHA256(authorizedPubKey))
	}

	dlog.Common.Debug(user, "Offered public key fingerprint",
		gossh.FingerprintSHA256(offeredPubKey))
	if authorizedKeysMap[string(offeredPubKey.Marshal())] {
		return &gossh.Permissions{
			Extensions: map[string]string{
				"pubkey-fp": gossh.FingerprintSHA256(offeredPubKey),
			},
		}, nil
	}

	return nil, fmt.Errorf("%s|Public key of user not authorized", user)
}
