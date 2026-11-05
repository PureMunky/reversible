package decision

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// RecordType names what a log record does.
type RecordType string

const (
	RecordNode      RecordType = "node"
	RecordExecution RecordType = "execution"
	RecordRewind    RecordType = "rewind"
)

// Record is one append-only entry in the decision log. The graph is
// rebuilt by replaying records in order; nothing is ever edited in place.
type Record struct {
	Type     RecordType `json:"type"`
	Time     time.Time  `json:"time"`
	Node     *Node      `json:"node,omitempty"`
	NodeID   string     `json:"node_id,omitempty"`
	IntentID string     `json:"intent_id,omitempty"`
	Actor    string     `json:"actor,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	RewindOf string     `json:"rewind_of,omitempty"`

	// Set by ChainLog: position, link to the previous record, this
	// record's hash, and the signature of whoever is accountable for it.
	Seq    int    `json:"seq,omitempty"`
	Prev   string `json:"prev,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Signer string `json:"signer,omitempty"`
	Sig    string `json:"sig,omitempty"`
}

// Log stores decision records.
type Log interface {
	Append(Record) error
	Load() ([]Record, error)
}

// MemoryLog keeps records in memory.
type MemoryLog struct {
	mu      sync.Mutex
	records []Record
}

func NewMemoryLog() *MemoryLog { return &MemoryLog{} }

func (l *MemoryLog) Append(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Node != nil {
		n := *r.Node
		r.Node = &n
	}
	l.records = append(l.records, r)
	return nil
}

func (l *MemoryLog) Load() ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Record(nil), l.records...), nil
}

// FileLog appends records as JSON lines, fsyncing each one.
type FileLog struct {
	mu   sync.Mutex
	path string
}

func NewFileLog(path string) *FileLog { return &FileLog{path: path} }

func (l *FileLog) Append(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (l *FileLog) Load() ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
