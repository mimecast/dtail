package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// JournalSpecPrefix marks a read target as a systemd journal source.
const JournalSpecPrefix = "journal:"

// ReadTargetKind identifies the backing source for a validated read target.
type ReadTargetKind int

// Valid read target kinds.
const (
	FileKind ReadTargetKind = iota
	JournalKind
)

// ValidatedReadTarget stores a resolved regular file path for rooted re-opens.
type ValidatedReadTarget struct {
	Kind         ReadTargetKind
	resolvedPath string
	rootedPath   RootedPath
}

// IsJournalSpec reports whether spec names a journal-backed read source.
func IsJournalSpec(spec string) bool {
	return strings.HasPrefix(spec, JournalSpecPrefix)
}

// NewValidatedReadTarget returns a rooted target for a resolved regular file.
func NewValidatedReadTarget(resolvedPath string) (ValidatedReadTarget, error) {
	cleanedPath := filepath.Clean(resolvedPath)
	if !filepath.IsAbs(cleanedPath) {
		return ValidatedReadTarget{}, fmt.Errorf("validated read target requires absolute path: %s", cleanedPath)
	}

	info, err := os.Lstat(cleanedPath)
	if err != nil {
		return ValidatedReadTarget{}, fmt.Errorf("lstat validated read target %s: %w", cleanedPath, err)
	}
	if !info.Mode().IsRegular() {
		return ValidatedReadTarget{}, fmt.Errorf("validated read target must be a regular file: %s", cleanedPath)
	}

	rootedPath, err := NewRootedPath(cleanedPath)
	if err != nil {
		return ValidatedReadTarget{}, err
	}

	return ValidatedReadTarget{
		Kind:         FileKind,
		resolvedPath: cleanedPath,
		rootedPath:   rootedPath,
	}, nil
}

// NewValidatedJournalTarget returns a validated journal-backed read target.
func NewValidatedJournalTarget(spec string) (ValidatedReadTarget, error) {
	if !IsJournalSpec(spec) {
		return ValidatedReadTarget{}, fmt.Errorf("journal read target requires %q prefix: %s", JournalSpecPrefix, spec)
	}
	if err := validateJournalSpec(spec); err != nil {
		return ValidatedReadTarget{}, err
	}
	return ValidatedReadTarget{
		Kind:         JournalKind,
		resolvedPath: spec,
	}, nil
}

func validateJournalSpec(spec string) error {
	source := strings.TrimPrefix(spec, JournalSpecPrefix)
	if source == "" {
		return fmt.Errorf("journal read target requires a unit name after %q", JournalSpecPrefix)
	}
	if strings.HasPrefix(source, "-") {
		return fmt.Errorf("journal read target unit must not start with '-'")
	}
	for _, r := range source {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("journal read target unit contains invalid whitespace or control character")
		}
	}
	return nil
}

// Open re-opens the validated file beneath its resolved parent directory.
func (t ValidatedReadTarget) Open() (*os.File, error) {
	if t.Kind != FileKind {
		return nil, fmt.Errorf("read target kind %d cannot be opened as a file", t.Kind)
	}

	root, rootErr := t.rootedPath.OpenRoot()
	if rootErr != nil {
		return nil, fmt.Errorf("open root for %s: %w", t.resolvedPath, rootErr)
	}
	defer func() { _ = root.Close() }()

	if err := t.validateEntry(root); err != nil {
		return nil, err
	}

	fd, err := root.Open(t.rootedPath.Name())
	if err != nil {
		return nil, fmt.Errorf("open rooted file %s: %w", t.resolvedPath, err)
	}

	if err := validateOpenedFile(fd, t.resolvedPath); err != nil {
		if closeErr := fd.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close invalid rooted file %s: %w", t.resolvedPath, closeErr))
		}
		return nil, err
	}
	if err := t.validateEntry(root); err != nil {
		if closeErr := fd.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close changed rooted file %s: %w", t.resolvedPath, closeErr))
		}
		return nil, err
	}

	return fd, nil
}

func (t ValidatedReadTarget) validateEntry(root *os.Root) error {
	info, err := root.Lstat(t.rootedPath.Name())
	if err != nil {
		return fmt.Errorf("lstat rooted file %s: %w", t.resolvedPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rooted file changed to symlink: %s", t.resolvedPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("rooted file changed to non-regular file: %s", t.resolvedPath)
	}
	return nil
}

func validateOpenedFile(fd *os.File, resolvedPath string) error {
	info, err := fd.Stat()
	if err != nil {
		return fmt.Errorf("stat opened file %s: %w", resolvedPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opened file is not regular: %s", resolvedPath)
	}
	return nil
}
