package integrationtests

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runCommand(t *testing.T, cmd string, args []string, stdoutFile string) error {
	if _, err := os.Stat(cmd); err != nil {
		return fmt.Errorf("No such binary %s, please compile first (%v)", cmd, err)
	}

	t.Log("Executing command:", cmd, strings.Join(args, " "))
	bytes, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return err
	}

	t.Log("Writing stdout to file", stdoutFile)
	fd, err := os.Create(stdoutFile)
	if err != nil {
		return err
	}
	fd.Write(bytes)
	fd.Close()

	return nil
}

func compareFiles(t *testing.T, fileA, fileB string) error {
	t.Log("Comparing files", fileA, fileB)
	shaFileA := shaOfFile(t, fileA)
	shaFileB := shaOfFile(t, fileB)

	if shaFileA != shaFileB {
		t.Errorf("Expected SHA %s but got %s", shaFileA, shaFileB)
		if bytes, err := exec.Command("diff", "-u", fileA, fileB).Output(); err != nil {
			return fmt.Errorf(string(bytes))
		}
	}

	return nil
}

func shaOfFile(t *testing.T, file string) string {
	bytes, err := ioutil.ReadFile(file)
	if err != nil {
		t.Error(err)
	}
	hasher := sha256.New()
	hasher.Write(bytes)
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))
	t.Log("SHA", file, sha)
	return sha
}
