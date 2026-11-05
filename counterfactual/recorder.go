package counterfactual

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/PureMunky/reversible"
)

// Recorder captures real requests so candidate programs can be replayed
// against them later.
type Recorder interface {
	Record(Request) error
	Requests() ([]Request, error)
}

// MemoryRecorder keeps requests in memory.
type MemoryRecorder struct {
	mu   sync.Mutex
	reqs []Request
}

func NewMemoryRecorder() *MemoryRecorder { return &MemoryRecorder{} }

func (m *MemoryRecorder) Record(r Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, r)
	return nil
}

func (m *MemoryRecorder) Requests() ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.reqs...), nil
}

// FileRecorder appends requests as JSON lines.
type FileRecorder struct {
	mu   sync.Mutex
	path string
}

func NewFileRecorder(path string) *FileRecorder { return &FileRecorder{path: path} }

func (f *FileRecorder) Record(r Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(b, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (f *FileRecorder) Requests() ([]Request, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []Request
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var r Request
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Recording wraps a Program so every request it handles is recorded first.
// Put it in front of the live program to build up replayable traffic.
func Recording(p Program, rec Recorder) Program {
	return func(ctx context.Context, req Request) (reversible.Intent, error) {
		if err := rec.Record(req); err != nil {
			return reversible.Intent{}, err
		}
		return p(ctx, req)
	}
}
