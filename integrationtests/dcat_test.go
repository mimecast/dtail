package integrationtests

import (
	"context"
	"os"
	"testing"

	"github.com/mimecast/dtail/internal/config"
)

func TestDCat1(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		t.Log("Skipping")
		return
	}

	inFiles := []string{"dcat1a.txt", "dcat1b.txt", "dcat1c.txt", "dcat1d.txt"}
	for _, inFile := range inFiles {
		if err := testDCat1(t, inFile); err != nil {
			t.Error(err)
			return
		}
	}
}

func testDCat1(t *testing.T, inFile string) error {
	outFile := "dcat1.out"

	_, err := runCommand(context.TODO(), t, outFile,
		"../dcat", "--plain", "--cfg", "none", inFile)
	if err != nil {
		return err
	}
	if err := compareFiles(t, outFile, inFile); err != nil {
		return err
	}

	os.Remove(outFile)
	return nil
}

func TestDCat2(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}
	inFile := "dcat2.txt"
	expectedFile := "dcat2.txt.expected"
	outFile := "dcat2.out"

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none"}

	// Cat file 100 times in one session.
	for i := 0; i < 100; i++ {
		args = append(args, inFile)
	}

	_, err := runCommand(context.TODO(), t, outFile, "../dcat", args...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContents(t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(outFile)
}

func TestDCat3(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}
	inFile := "dcat3.txt"
	expectedFile := "dcat3.txt.expected"
	outFile := "dcat3.out"

	args := []string{"--plain", "--logLevel", "error", "--cfg", "none", inFile}

	// Split up long lines to smaller ones.
	os.Setenv("DTAIL_MAX_LINE_LENGTH", "1000")

	_, err := runCommand(context.TODO(), t, outFile, "../dcat", args...)
	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFilesContents(t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(outFile)
}

func TestDCatColors(t *testing.T) {
	if !config.Env("DTAIL_INTEGRATION_TEST_RUN_MODE") {
		return
	}

	inFile := "dcatcolors.txt"
	outFile := "dcatcolors.out"
	expectedFile := "dcatcolors.expected"

	_, err := runCommand(context.TODO(), t, outFile,
		"../dcat", "--logLevel", "error", "--cfg", "none", inFile)

	if err != nil {
		t.Error(err)
		return
	}

	if err := compareFiles(t, outFile, expectedFile); err != nil {
		t.Error(err)
		return
	}

	os.Remove(outFile)
}
