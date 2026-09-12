package gitx

import (
	"os"
	"path/filepath"
)

// ResolvePath returns an absolute, symlink-free path. macOS resolves /tmp and
// /var through symlinks, and git always reports the resolved form, so comparing
// a user-supplied path against git's output without this produces false
// mismatches.
func ResolvePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// The path may not exist yet; resolve the deepest existing ancestor so the
	// comparison is still meaningful.
	dir, base := filepath.Split(abs)
	if dir == "" || dir == abs {
		return abs
	}
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(resolvedDir, base)
	}
	return filepath.Clean(abs)
}

// SamePath reports whether two paths refer to the same location.
func SamePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	return ResolvePath(a) == ResolvePath(b)
}

// DirExists reports whether path is an existing directory.
func DirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// DirIsEmpty reports whether path is a directory with no entries.
func DirIsEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}
