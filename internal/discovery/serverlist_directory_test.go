package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory named like the server value is no server list file: the value
// is the host of that name.
func TestIsServerListFileIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "web1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "servers.txt"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if IsServerListFile("web1") {
		t.Fatal("a directory was taken for a server list file")
	}
	if !IsServerListFile("servers.txt") {
		t.Fatal("a regular file was not taken for a server list file")
	}
	d, err := New("", "web1", Shuffle, nil)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := d.ServerList()
	if err != nil || len(servers) != 1 || servers[0] != "web1" {
		t.Fatalf("ServerList() = %v, %v; want the host web1", servers, err)
	}
}
