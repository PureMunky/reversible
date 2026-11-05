package reversible

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// EventKind names what happened to an intent or effect.
type EventKind string

const (
	EventDeclared           EventKind = "declared"
	EventApproved           EventKind = "approved"
	EventDenied             EventKind = "denied"
	EventExecuted           EventKind = "executed"
	EventFailed             EventKind = "failed"
	EventHeld               EventKind = "held"
	EventReleased           EventKind = "released"
	EventCancelled          EventKind = "cancelled"
	EventCompensated        EventKind = "compensated"
	EventCompensationFailed EventKind = "compensation_failed"
	EventStranded           EventKind = "stranded"
	EventUncertain          EventKind = "uncertain"
	EventRecovered          EventKind = "recovered"
)

// Event is one append-only ledger entry. An empty Effect means the event is
// about the intent as a whole.
type Event struct {
	Time     time.Time `json:"time"`
	IntentID string    `json:"intent"`
	Goal     string    `json:"goal,omitempty"`
	Effect   string    `json:"effect,omitempty"`
	Class    string    `json:"class,omitempty"`
	Kind     EventKind `json:"kind"`
	Receipt  Receipt   `json:"receipt,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Ledger is an append-only record of everything the runtime did and why.
type Ledger interface {
	Append(Event) error
	Events(intentID string) ([]Event, error)
}

// MemoryLedger keeps events in memory. Useful for tests and demos.
type MemoryLedger struct {
	mu     sync.Mutex
	events []Event
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{} }

func (l *MemoryLedger) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

// Events returns events for one intent, or all events if intentID is "".
func (l *MemoryLedger) Events(intentID string) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if intentID == "" || e.IntentID == intentID {
			out = append(out, e)
		}
	}
	return out, nil
}

// FileLedger appends events as JSON lines to a file.
type FileLedger struct {
	mu   sync.Mutex
	path string
}

func NewFileLedger(path string) *FileLedger { return &FileLedger{path: path} }

func (l *FileLedger) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l *FileLedger) Events(intentID string) ([]Event, error) {
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
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		if intentID == "" || e.IntentID == intentID {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
