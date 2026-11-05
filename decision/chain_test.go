package decision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func signedGraph(t *testing.T, path string) (*Graph, *Keyring) {
	t.Helper()
	keys := NewKeyring()
	for _, a := range []string{"phil", "pricing-agent", "growth-agent"} {
		if _, err := keys.Generate(a); err != nil {
			t.Fatal(err)
		}
	}
	keys.Default = "phil"
	cl, err := NewChainLog(NewFileLog(path), keys)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Open(cl)
	if err != nil {
		t.Fatal(err)
	}
	return g, keys
}

func TestSignedChainRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	g, keys := signedGraph(t, path)
	_, rt, w := setup(t, NewMemoryLog())
	buildStory(t, g, rt, w)
	g.Rewind(context.Background(), "annual-discount", rt, "too generous", "phil")

	// A verifier with only public keys can reopen and check everything.
	cl, err := NewChainLog(NewFileLog(path), keys.PublicKeys())
	if err != nil {
		t.Fatal(err)
	}
	g2, err := Open(cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Nodes()) != 5 {
		t.Fatalf("nodes = %d", len(g2.Nodes()))
	}
	records, _ := NewFileLog(path).Load()
	signers := map[string]bool{}
	for _, r := range records {
		signers[r.Signer] = true
	}
	if !signers["phil"] || !signers["pricing-agent"] || !signers["growth-agent"] {
		t.Fatalf("signers = %v", signers)
	}
}

func TestUnsignedActorRefused(t *testing.T) {
	g, _ := signedGraph(t, filepath.Join(t.TempDir(), "d.jsonl"))
	g.Goal("g", "goal", "phil")
	_, err := g.Decide(Decision{ID: "d", Parents: []string{"g"}, Actor: "rogue-agent", Summary: "x"})
	if err == nil || !strings.Contains(err.Error(), "no signing key") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := g.Get("d"); ok {
		t.Fatal("refused decision was still added to the graph")
	}
}

// tamper rewrites the log file, applying fn to the decoded records.
func tamper(t *testing.T, path string, fn func([]map[string]any) []map[string]any) {
	t.Helper()
	b, _ := os.ReadFile(path)
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		recs = append(recs, m)
	}
	recs = fn(recs)
	var out []string
	for _, m := range recs {
		l, _ := json.Marshal(m)
		out = append(out, string(l))
	}
	os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func TestTamperingIsDetected(t *testing.T) {
	cases := map[string]struct {
		fn   func([]map[string]any) []map[string]any
		want string
	}{
		"edit a rationale": {func(r []map[string]any) []map[string]any {
			r[1]["node"].(map[string]any)["rationale"] = "because the CEO said so"
			return r
		}, "altered"},
		"delete a record": {func(r []map[string]any) []map[string]any {
			return append(r[:2], r[3:]...)
		}, "sequence"},
		"reorder records": {func(r []map[string]any) []map[string]any {
			r[1], r[2] = r[2], r[1]
			return r
		}, "sequence"},
		"blame someone else": {func(r []map[string]any) []map[string]any {
			r[1]["actor"] = "growth-agent"
			r[1]["node"].(map[string]any)["actor"] = "growth-agent"
			return r
		}, "altered"},
		"re-sign as someone else without their key": {func(r []map[string]any) []map[string]any {
			r[1]["signer"] = "phil"
			return r
		}, "altered"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d.jsonl")
			g, keys := signedGraph(t, path)
			_, rt, w := setup(t, NewMemoryLog())
			buildStory(t, g, rt, w)
			tamper(t, path, tc.fn)

			_, err := NewChainLog(NewFileLog(path), keys.PublicKeys())
			if !errors.Is(err, ErrChain) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrChain mentioning %q", err, tc.want)
			}
		})
	}
}

func TestForgedSignatureWithWrongKeyDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	g, keys := signedGraph(t, path)
	g.Goal("g", "goal", "phil")

	// An attacker with their own keyring appends a record claiming to be phil.
	evil := NewKeyring()
	evil.Generate("phil")
	cl, err := NewChainLog(NewFileLog(path), mergeForAttack(keys, evil))
	if err != nil {
		t.Fatal(err)
	}
	cl.Append(Record{Type: RecordRewind, NodeID: "g", Actor: "phil", Reason: "forged"})

	_, err = NewChainLog(NewFileLog(path), keys.PublicKeys())
	if !errors.Is(err, ErrChain) || !strings.Contains(err.Error(), "signature is not valid") {
		t.Fatalf("err = %v", err)
	}
}

// mergeForAttack trusts the real public keys (so the existing chain
// verifies) but signs with the attacker's private key.
func mergeForAttack(real, evil *Keyring) *Keyring {
	k := NewKeyring()
	for a, p := range real.public {
		k.public[a] = p
	}
	for a, p := range evil.private {
		k.private[a] = p
	}
	return k
}

func TestMemoryLogLoadReturnsCopies(t *testing.T) {
	log := NewMemoryLog()
	g, _ := Open(log)
	g.Goal("g", "original", "phil")
	recs, _ := log.Load()
	recs[0].Node.Summary = "rewritten"
	again, _ := log.Load()
	if again[0].Node.Summary != "original" {
		t.Fatal("Load leaked a pointer into stored history")
	}
}
