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

func mapFile(t *testing.T, file string) (map[string]int, error) {
	t.Log("Mapping", file)
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

// Checks whether both files have the same lines (order doesn't matter)
func compareFilesContents(t *testing.T, fileA, fileB string) error {
	compareMaps := func(a, b map[string]int) error {
		for line, countA := range a {
			countB, ok := b[line]
			if !ok {
				return fmt.Errorf("Files differ, line '%s' is missing in one of them", line)
			}
			if countA != countB {
				return fmt.Errorf("Files differ, count of line '%s' is %d in one but %d in another",
					line, countA, countB)
			}
		}
		return nil
	}

	// Read files into maps.
	a, err := mapFile(t, fileA)
	if err != nil {
		return err
	}
	b, err := mapFile(t, fileB)
	if err != nil {
		return err
	}

	// The mapreduce result can be in a different order each time (Golang maps are not sorted).
	t.Log(fmt.Sprintf("Checking whether %s has same lines as file %s (ignoring line order)",
		fileA, fileB))
	if err := compareMaps(a, b); err != nil {
		return err
	}
	t.Log(fmt.Sprintf("Checking whether %s has same lines as file %s (ignoring line order)",
		fileB, fileA))
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
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Expected SHA %s but got %s:\n", shaFileA, shaFileB))
		if bytes, err := exec.Command("diff", "-u", fileA, fileB).Output(); err != nil {
			sb.Write(bytes)
		}
		return fmt.Errorf(sb.String())
	}

	return nil
}

func fileContainsStr(t *testing.T, file, str string) error {
	t.Log("Checking if file contains string", file, str)
	m, err := mapFile(t, file)
	if err != nil {
		return err
	}

	for line := range m {
		if strings.Contains(line, str) {
			t.Log(line)
			return nil
		}
	}

	return fmt.Errorf("File %s does not contain string %s", file, str)
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
