//go:build linuxacl
// +build linuxacl

package permissions

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

const (
	setfacl string = "/usr/bin/setfacl"
	file    string = "/tmp/acltest"
)

func TestLinuxACL(t *testing.T) {
	setfacl := "/usr/bin/setfacl"
	file := "/tmp/acltest"

	// Delete file if it exists.
	if _, err := os.Stat(file); err == nil {
		os.Remove(file)
	}

	f, err := os.Create(file)
	if err != nil {
		t.Errorf("%v", err)
	}
	defer func() {
		f.Close()
		//os.Remove(file)
	}()

	user, err := user.Current()
	if err != nil {
		t.Errorf("Unable to retrieve current user: %v", err)
	}

	// Test 1: Remove all permissions and perform a permission check
	cmd := exec.Command(setfacl, "-b", "-m", "u::---,g::---,o::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, _ := ToRead(user.Username, file); ok {
		t.Errorf("Didn't expect permissions to read file!")
	}

	// Test 2: Add read permission to file owner
	cmd = exec.Command(setfacl, "-b", "-m", "u::r--,g::---,o::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, err := ToRead(user.Username, file); !ok {
		t.Errorf("Expected permissions to read file: %v", err)
	}

	// Test 3: Add read permission to file group
	cmd = exec.Command(setfacl, "-b", "-m", "u::---,g::r--,o::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, err := ToRead(user.Username, file); !ok {
		t.Errorf("Expected permissions to read file: %v", err)
	}

	// Test 4: Add read permission to others
	cmd = exec.Command(setfacl, "-b", "-m", "u::---,g::---,o::r--", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}

	if ok, err := ToRead(user.Username, file); !ok {
		t.Errorf("Expected permissions to read file: %v", err)
	}

	// Test 5: Remove read permission from mask
	cmd = exec.Command(setfacl, "-m", "m::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, _ := ToRead(user.Username, file); ok {
		t.Errorf("Didn't expect permissions to read file!")
	}
	cmd = exec.Command(setfacl, "-m", "m::r--", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}

	// Test 6: Add read permission to specific group
	cmd = exec.Command(setfacl, "-b", "-m", "u::---,g:"+user.Username+":r--,o::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, err := ToRead(user.Username, file); !ok {
		t.Errorf("Expected permissions to read file for user %v: %v", user.Username, err)
	}

	// Test 7: Remove all permissions but mask
	cmd = exec.Command(setfacl, "-b", "-m", "u::---,g::---,o::---", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	cmd = exec.Command(setfacl, "-m", "m::r--", file)
	if err := cmd.Run(); err != nil {
		t.Errorf("%s -> %v", strings.Join(cmd.Args, " "), err)
	}
	if ok, _ := ToRead(user.Username, file); ok {
		t.Errorf("Didn't expect permissions to read file!")
	}
}
