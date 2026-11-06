package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Recordings let a run that needs a model be repeated without one: a
// Recorder saves every reply under a key derived from the exact system
// prompt and prompt that produced it; a Replayer answers the same prompts
// with the saved replies, and refuses any other prompt. Offline demos and
// CI replay recorded replies; a changed spec changes the prompt, so a
// stale recording can never stand in for a new one.

// RecordingKey names the reply to a prompt.
func RecordingKey(system, prompt string) string {
	h := sha256.Sum256([]byte(system + "\x00" + prompt))
	return hex.EncodeToString(h[:12])
}

// ErrNotRecorded means no reply was recorded for this exact prompt.
var ErrNotRecorded = errors.New("no recorded reply for this prompt")

// Replayer answers from recorded replies (files named <key>.reply).
type Replayer struct {
	FS fs.FS
}

func (r Replayer) Complete(_ context.Context, system, prompt string) (string, error) {
	key := RecordingKey(system, prompt)
	b, err := fs.ReadFile(r.FS, key+".reply")
	if err != nil {
		return "", fmt.Errorf("%w (key %s): the prompt changed since the recording; run live with recording on to make a new one", ErrNotRecorded, key)
	}
	return string(b), nil
}

// Recorder passes calls to a model and saves each reply in Dir.
type Recorder struct {
	Model Completer
	Dir   string
}

func (r Recorder) Complete(ctx context.Context, system, prompt string) (string, error) {
	reply, err := r.Model.Complete(ctx, system, prompt)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return "", err
	}
	key := RecordingKey(system, prompt)
	if err := os.WriteFile(filepath.Join(r.Dir, key+".reply"), []byte(reply), 0o644); err != nil {
		return "", err
	}
	return reply, nil
}
