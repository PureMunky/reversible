package reversible

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StepSnapshot is the persisted state of one effect.
type StepSnapshot struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Class       Class     `json:"class"`
	Spec        *Spec     `json:"spec"`
	State       StepState `json:"state"`
	Receipt     Receipt   `json:"receipt,omitempty"`
	ReleaseAt   time.Time `json:"release_at,omitzero"`
}

// IntentSnapshot is the persisted state of an intent. It is everything a
// fresh process needs to resume holding, releasing or undoing it.
type IntentSnapshot struct {
	ID        string         `json:"id"`
	Goal      string         `json:"goal,omitempty"`
	Status    Status         `json:"status"`
	Reason    string         `json:"reason,omitempty"`
	Steps     []StepSnapshot `json:"steps"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Store persists intent state. Save is called before each effect runs
// (write-ahead) and after each state change; if it fails before an effect
// runs, the effect does not run.
type Store interface {
	Save(IntentSnapshot) error
	LoadAll() ([]IntentSnapshot, error)
}

// ErrLocked is returned when another process holds a store's lock.
var ErrLocked = errors.New("store is locked by another process")

// Locker is implemented by stores that can be held exclusively. A Runtime
// with such a store takes the lock in Recover and releases it in Close, so
// two processes can never act on the same intents at once.
type Locker interface {
	Lock() (unlock func() error, err error)
}

// FileStore keeps one JSON file per intent in a directory, written
// atomically (temp file + rename).
type FileStore struct{ dir string }

// Lock takes an exclusive, non-blocking lock on the store directory.
// It returns ErrLocked if another process holds it.
func (s *FileStore) Lock() (func() error, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", s.dir, err)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "pid %d\n", os.Getpid())
	return func() error {
		err := unlockFile(f)
		return errors.Join(err, f.Close())
	}, nil
}

// NewFileStore creates the directory if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(id string) string {
	return filepath.Join(s.dir, url.PathEscape(id)+".intent.json")
}

func (s *FileStore) Save(snap IntentSnapshot) error {
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(snap.ID))
}

func (s *FileStore) LoadAll() ([]IntentSnapshot, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []IntentSnapshot
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".intent.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var snap IntentSnapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out, errors.Join(errs...)
}
