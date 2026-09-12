package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/adrg/xdg"
	"github.com/gofrs/flock"

	"github.com/idosaban-scaleops/flow/internal/config"
)

// Store reads and writes the registry file, serializing concurrent flow
// invocations with a lock on a sibling .lock file.
type Store struct {
	path string
	// lock guards against other flow processes; mu guards against goroutines
	// inside this one, since a flock is held per file descriptor and so is not
	// re-entrant-safe when one Store is shared.
	lock *flock.Flock
	mu   sync.Mutex
}

// DefaultPath returns the registry location, honoring FLOW_REGISTRY for tests.
func DefaultPath() string {
	if p := os.Getenv("FLOW_REGISTRY"); p != "" {
		return config.ExpandPath(p)
	}
	return filepath.Join(xdg.DataHome, "flow", "registry.json")
}

// Open returns a Store for path, or the default location when path is empty.
func Open(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return &Store{path: path, lock: flock.New(path + ".lock")}
}

// Path reports the registry file location.
func (s *Store) Path() string { return s.path }

// Load reads the registry. A missing file yields an empty registry, because a
// first run is not an error.
func (s *Store) Load() (*File, error) {
	data, err := os.ReadFile(s.path) //nolint:gosec // path is flow's own data file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &File{Version: Version}, nil
	case err != nil:
		return nil, fmt.Errorf("read registry %s: %w", s.path, err)
	}
	return s.decode(data)
}

func (s *Store) decode(data []byte) (*File, error) {
	if len(data) == 0 {
		return &File{Version: Version}, nil
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", s.path, err)
	}
	if f.Version == 0 {
		f.Version = Version
	}
	if f.Version != Version {
		return nil, &ErrUnknownVersion{Path: s.path, Got: f.Version}
	}
	return &f, nil
}

// Update performs a locked read-modify-write. The lock is held for the whole
// cycle, so two concurrent flow processes cannot interleave and lose an entry.
func (s *Store) Update(fn func(*File) error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.lock.Lock(); err != nil {
		return fmt.Errorf("lock registry: %w", err)
	}
	defer func() { _ = s.lock.Unlock() }()

	f, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		return err
	}
	return s.save(f)
}

func (s *Store) save(f *File) error {
	f.Version = Version
	if f.Entries == nil {
		f.Entries = []Entry{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	data = append(data, '\n')
	if err := config.WriteFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}
