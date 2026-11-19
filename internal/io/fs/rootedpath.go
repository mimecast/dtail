package fs

import (
	"fmt"
	"os"
	"path/filepath"
)

// RootedPath scopes file operations to the path's parent directory via os.Root.
type RootedPath struct {
	path     string
	rootDir  string
	rootName string
}

// NewRootedPath returns a file path split into a trusted parent directory root
// and a relative leaf name for os.Root operations.
func NewRootedPath(path string) (RootedPath, error) {
	if path == "" {
		return RootedPath{}, fmt.Errorf("rooted path requires non-empty path")
	}

	cleanedPath := filepath.Clean(path)
	if cleanedPath == "." || cleanedPath == string(filepath.Separator) {
		return RootedPath{}, fmt.Errorf("rooted path requires a file path: %s", path)
	}

	return RootedPath{
		path:     path,
		rootDir:  filepath.Dir(cleanedPath),
		rootName: filepath.Base(cleanedPath),
	}, nil
}

// Name returns the rooted leaf name.
func (p RootedPath) Name() string {
	return p.rootName
}

// Path returns the original file path.
func (p RootedPath) Path() string {
	return p.path
}

// OpenRoot opens the rooted parent directory for this file path.
func (p RootedPath) OpenRoot() (*os.Root, error) {
	root, err := os.OpenRoot(p.rootDir)
	if err != nil {
		return nil, fmt.Errorf("open root for %s: %w", p.path, err)
	}
	return root, nil
}

// ReadFile reads the rooted file contents.
func (p RootedPath) ReadFile() ([]byte, error) {
	root, err := p.OpenRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	data, err := root.ReadFile(p.rootName)
	if err != nil {
		return nil, fmt.Errorf("read rooted file %s: %w", p.path, err)
	}
	return data, nil
}

// Stat stats the rooted file.
func (p RootedPath) Stat() (os.FileInfo, error) {
	root, err := p.OpenRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	info, err := root.Stat(p.rootName)
	if err != nil {
		return nil, fmt.Errorf("stat rooted file %s: %w", p.path, err)
	}
	return info, nil
}

// WriteFile writes the rooted file contents.
func (p RootedPath) WriteFile(data []byte, perm os.FileMode) error {
	root, err := p.OpenRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := root.WriteFile(p.rootName, data, perm); err != nil {
		return fmt.Errorf("write rooted file %s: %w", p.path, err)
	}
	return nil
}
