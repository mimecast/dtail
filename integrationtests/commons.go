package integrationtests

import (
	"bufio"
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

// Checks whether both files have the same lines (order doesn't matter)
func compareFilesContents(t *testing.T, fileA, fileB string) error {
	mapFile := func(file string) (map[string]int, error) {
		t.Log("Reading", file)
		contents := make(map[string]int)
		fd, err := os.Open(file)
		if err != nil {
			return contents, err
		}
		defer fd.Close()

		scanner := bufio.NewScanner(fd)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			line := scanner.Text()
			count, _ := contents[line]
			contents[line] = count + 1
		}

		return contents, nil
	}

	compareMaps := func(a, b map[string]int) error {
		for line, countA := range a {
			countB, ok := b[line]
			if !ok {
				return fmt.Errorf("Files differ, line '%s' is missing in one of them", line)
			}
			if countA != countB {
				return fmt.Errorf("Files differ, count of line '%s' is %d in one but %d in another", line, countA, countB)
			}
		}
		return nil
	}

	a, err := mapFile(fileA)
	if err != nil {
		return err
	}
	b, err := mapFile(fileB)
	if err != nil {
		return err
	}

	if err := compareMaps(a, b); err != nil {
		return err
	}
	if err := compareMaps(b, a); err != nil {
		return err
	}

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
