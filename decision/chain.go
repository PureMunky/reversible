package decision

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// The accountability chain makes the decision log tamper-evident and
// attributable. Every record is hash-linked to the one before it and signed
// with the ed25519 key of the actor responsible for it (the person who set
// a goal or rewound a decision, the agent that decided). Changing, removing
// or reordering any record, or forging an actor, breaks verification.

// Keyring holds actors' keys. Private keys are needed to append; public keys
// are enough to verify.
type Keyring struct {
	mu      sync.RWMutex
	private map[string]ed25519.PrivateKey
	public  map[string]ed25519.PublicKey
	// Default signs records with no actor (for example, an outcome
	// recorded by a monitoring job). Empty means such records are refused.
	Default string
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring {
	return &Keyring{private: map[string]ed25519.PrivateKey{}, public: map[string]ed25519.PublicKey{}}
}

// Generate creates and stores a new key for an actor, returning its
// public half.
func (k *Keyring) Generate(actor string) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.private[actor], k.public[actor] = priv, pub
	return pub, nil
}

// Trust adds an actor's public key, for verifying records signed elsewhere.
func (k *Keyring) Trust(actor string, pub ed25519.PublicKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.public[actor] = pub
}

// PublicKeys returns a verify-only copy of the keyring.
func (k *Keyring) PublicKeys() *Keyring {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := NewKeyring()
	out.Default = k.Default
	for a, p := range k.public {
		out.public[a] = p
	}
	return out
}

func (k *Keyring) signer(actor string) (string, ed25519.PrivateKey, error) {
	if actor == "" {
		actor = k.Default
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	priv, ok := k.private[actor]
	if !ok {
		if actor == "" {
			return "", nil, errors.New("record has no actor and the keyring has no default signer")
		}
		return "", nil, fmt.Errorf("actor %q has no signing key; unsigned decisions are refused", actor)
	}
	return actor, priv, nil
}

func (k *Keyring) verifier(actor string) (ed25519.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	p, ok := k.public[actor]
	return p, ok
}

// ChainLog wraps a Log, linking and signing every record it appends.
type ChainLog struct {
	mu    sync.Mutex
	inner Log
	keys  *Keyring
	seq   int
	last  string
}

// NewChainLog wraps inner. Existing records are verified first; a broken
// chain is refused rather than extended.
func NewChainLog(inner Log, keys *Keyring) (*ChainLog, error) {
	records, err := inner.Load()
	if err != nil {
		return nil, err
	}
	if err := VerifyChain(records, keys); err != nil {
		return nil, err
	}
	c := &ChainLog{inner: inner, keys: keys}
	if n := len(records); n > 0 {
		c.seq, c.last = records[n-1].Seq, records[n-1].Hash
	}
	return c, nil
}

// Append links, hashes and signs the record, then appends it.
func (c *ChainLog) Append(r Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	signer, priv, err := c.keys.signer(r.Actor)
	if err != nil {
		return err
	}
	r.Seq, r.Prev, r.Signer = c.seq+1, c.last, signer
	h, err := recordHash(r)
	if err != nil {
		return err
	}
	r.Hash = h
	r.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(h)))
	if err := c.inner.Append(r); err != nil {
		return err
	}
	c.seq, c.last = r.Seq, r.Hash
	return nil
}

// Load returns the records after verifying the whole chain.
func (c *ChainLog) Load() ([]Record, error) {
	records, err := c.inner.Load()
	if err != nil {
		return nil, err
	}
	return records, VerifyChain(records, c.keys)
}

// ChainError says where and how a chain broke.
type ChainError struct {
	Seq    int // the record that failed (1-based); 0 if unknown
	Reason string
}

func (e *ChainError) Error() string {
	return fmt.Sprintf("decision log fails verification at record %d: %s", e.Seq, e.Reason)
}

// ErrChain matches any ChainError with errors.Is.
var ErrChain = errors.New("decision log fails verification")

func (e *ChainError) Is(target error) bool { return target == ErrChain }

// VerifyChain checks that records form an unbroken, correctly ordered hash
// chain and that every record is signed by the key of the actor it names.
func VerifyChain(records []Record, keys *Keyring) error {
	prev := ""
	for i, r := range records {
		want := i + 1
		if r.Seq != want {
			return &ChainError{Seq: want, Reason: fmt.Sprintf("sequence number is %d (records removed, duplicated or reordered)", r.Seq)}
		}
		if r.Prev != prev {
			return &ChainError{Seq: want, Reason: "does not link to the previous record"}
		}
		h, err := recordHash(r)
		if err != nil {
			return &ChainError{Seq: want, Reason: err.Error()}
		}
		if h != r.Hash {
			return &ChainError{Seq: want, Reason: "content does not match its hash (record was altered)"}
		}
		expect := r.Actor
		if expect == "" {
			expect = keys.Default
		}
		if r.Signer != expect {
			return &ChainError{Seq: want, Reason: fmt.Sprintf("signed by %q but the record names %q", r.Signer, expect)}
		}
		pub, ok := keys.verifier(r.Signer)
		if !ok {
			return &ChainError{Seq: want, Reason: fmt.Sprintf("no trusted key for signer %q", r.Signer)}
		}
		sig, err := base64.StdEncoding.DecodeString(r.Sig)
		if err != nil || !ed25519.Verify(pub, []byte(r.Hash), sig) {
			return &ChainError{Seq: want, Reason: fmt.Sprintf("signature is not valid for %q", r.Signer)}
		}
		prev = r.Hash
	}
	return nil
}

// recordHash hashes everything in the record except the hash and signature.
func recordHash(r Record) (string, error) {
	r.Hash, r.Sig = "", ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
