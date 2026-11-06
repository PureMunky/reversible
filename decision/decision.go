// Package decision is a causal ledger of why software changed.
//
// The effect runtime records *what* happened. This package records *why*:
//
//	goal ──▶ decision ──▶ decision ──▶ executions (reversible intents)
//	                 └──▶ outcome (what was observed afterwards)
//
// Every node names its parents, so any action can be traced back to the
// goal and the reasoning that produced it, and any decision can be
// rewound: the decision and everything downstream of it is undone, newest
// first, through the effect runtime.
package decision

import (
	"context"
	"errors"
	"fmt"
	"github.com/PureMunky/reversible/telemetry"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PureMunky/reversible"
)

// Kind is the type of a node in the graph.
type Kind string

const (
	KindGoal     Kind = "goal"
	KindDecision Kind = "decision"
	KindOutcome  Kind = "outcome"
)

// Node is one goal, decision or outcome.
type Node struct {
	ID      string    `json:"id"`
	Kind    Kind      `json:"kind"`
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor,omitempty"` // who decided: an agent name or a person
	Summary string    `json:"summary"`
	Parents []string  `json:"parents,omitempty"`

	// Decisions only.
	Rationale    string   `json:"rationale,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"` // options considered and not taken

	// Outcomes only.
	Metrics map[string]float64 `json:"metrics,omitempty"`

	// Filled in from later records.
	Executions   []string   `json:"executions,omitempty"` // reversible intent IDs
	RewoundAt    *time.Time `json:"rewound_at,omitempty"`
	RewoundBy    string     `json:"rewound_by,omitempty"`
	RewindReason string     `json:"rewind_reason,omitempty"`
	RewindOf     string     `json:"rewind_of,omitempty"` // the decision whose rewind swept this one up
}

// Rewound reports whether the node has been rewound.
func (n Node) Rewound() bool { return n.RewoundAt != nil }

// Undoer is the slice of the effect runtime a rewind needs.
// *reversible.Runtime implements it.
type Undoer interface {
	Undo(ctx context.Context, intentID, reason string) (*reversible.Result, error)
}

// Graph is the in-memory view of the ledger, rebuilt from its log.
type Graph struct {
	mu       sync.Mutex
	log      Log
	now      func() time.Time
	nodes    map[string]*Node
	order    []string
	children map[string][]string
}

// Option configures a Graph.
type Option func(*Graph)

// WithClock overrides the clock, mainly for tests.
func WithClock(now func() time.Time) Option { return func(g *Graph) { g.now = now } }

// Open loads a graph from its log.
func Open(log Log, opts ...Option) (*Graph, error) {
	g := &Graph{log: log, now: time.Now, nodes: map[string]*Node{}, children: map[string][]string{}}
	for _, o := range opts {
		o(g)
	}
	records, err := log.Load()
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if err := g.apply(r); err != nil {
			return nil, fmt.Errorf("replaying decision log: %w", err)
		}
	}
	return g, nil
}

// Goal records a goal: the root of a chain of decisions.
func (g *Graph) Goal(id, summary, actor string) (*Node, error) {
	return g.add(Node{ID: id, Kind: KindGoal, Summary: summary, Actor: actor})
}

// Decision is what Decide records.
type Decision struct {
	ID           string
	Parents      []string // a goal and/or earlier decisions
	Summary      string
	Rationale    string
	Alternatives []string
	Actor        string
}

// Decide records a decision. It must have at least one parent, so every
// decision traces back to a goal.
func (g *Graph) Decide(d Decision) (*Node, error) {
	if len(d.Parents) == 0 {
		return nil, fmt.Errorf("decision %q has no parent; every decision must trace back to a goal", d.ID)
	}
	return g.add(Node{ID: d.ID, Kind: KindDecision, Parents: d.Parents, Summary: d.Summary,
		Rationale: d.Rationale, Alternatives: d.Alternatives, Actor: d.Actor})
}

// Outcome is what Observe records.
type Outcome struct {
	ID      string
	Parents []string // the decisions this outcome is attributed to
	Summary string
	Metrics map[string]float64
	Actor   string
}

// Observe records an outcome attributed to one or more decisions.
func (g *Graph) Observe(o Outcome) (*Node, error) {
	if len(o.Parents) == 0 {
		return nil, fmt.Errorf("outcome %q is attributed to nothing", o.ID)
	}
	return g.add(Node{ID: o.ID, Kind: KindOutcome, Parents: o.Parents, Summary: o.Summary,
		Metrics: o.Metrics, Actor: o.Actor})
}

// Executed links a reversible intent to the decision that caused it.
func (g *Graph) Executed(decisionID, intentID string) error {
	return g.write(Record{Type: RecordExecution, NodeID: decisionID, IntentID: intentID})
}

// Execute runs an intent through the runtime and links it to a decision in
// one step. The link is written before the intent runs, so a crash can
// never leave an effect the ledger can't explain.
func (g *Graph) Execute(ctx context.Context, rt *reversible.Runtime, decisionID string, in reversible.Intent) (*reversible.Result, error) {
	if err := g.Executed(decisionID, in.ID); err != nil {
		return nil, err
	}
	return rt.Execute(ctx, in)
}

// Get returns a copy of a node.
func (g *Graph) Get(id string) (Node, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.nodes[id]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Nodes returns every node in the order it was recorded.
func (g *Graph) Nodes() []Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Node, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, *g.nodes[id])
	}
	return out
}

// Lineage returns the node and all its ancestors, root first.
func (g *Graph) Lineage(id string) ([]Node, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodes[id]; !ok {
		return nil, fmt.Errorf("unknown node %q", id)
	}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, p := range g.nodes[id].Parents {
			walk(p)
		}
	}
	walk(id)
	return g.inOrder(seen), nil
}

// Downstream returns the node and everything descended from it, in the
// order recorded (so parents always precede children).
func (g *Graph) Downstream(id string) ([]Node, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodes[id]; !ok {
		return nil, fmt.Errorf("unknown node %q", id)
	}
	return g.inOrder(g.downstreamSet(id)), nil
}

// ExplainIntent answers "why did this happen?" for a reversible intent.
func (g *Graph) ExplainIntent(intentID string) (string, error) {
	g.mu.Lock()
	var owner string
	for _, id := range g.order {
		for _, x := range g.nodes[id].Executions {
			if x == intentID {
				owner = id
			}
		}
	}
	g.mu.Unlock()
	if owner == "" {
		return "", fmt.Errorf("no decision is linked to intent %q", intentID)
	}
	why, err := g.Explain(owner)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("intent %s was executed because:\n%s", intentID, why), nil
}

// Explain renders the chain of reasoning behind a node, root first.
func (g *Graph) Explain(id string) (string, error) {
	lineage, err := g.Lineage(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, n := range lineage {
		indent := strings.Repeat("  ", i)
		fmt.Fprintf(&b, "%s%s %s: %s", indent, arrow(i), n.Kind, n.Summary)
		if n.Actor != "" {
			fmt.Fprintf(&b, " (%s)", n.Actor)
		}
		if n.Rewound() {
			fmt.Fprintf(&b, " [REWOUND: %s]", n.RewindReason)
		}
		b.WriteString("\n")
		if n.Rationale != "" {
			fmt.Fprintf(&b, "%s    because: %s\n", indent, n.Rationale)
		}
		if len(n.Alternatives) > 0 {
			fmt.Fprintf(&b, "%s    instead of: %s\n", indent, strings.Join(n.Alternatives, "; "))
		}
		if len(n.Metrics) > 0 {
			fmt.Fprintf(&b, "%s    observed: %s\n", indent, formatMetrics(n.Metrics))
		}
		if len(n.Executions) > 0 {
			shown, more := n.Executions, ""
			if len(shown) > 5 {
				shown, more = shown[:5], fmt.Sprintf(" (+%d more)", len(n.Executions)-5)
			}
			fmt.Fprintf(&b, "%s    executed: %s%s\n", indent, strings.Join(shown, ", "), more)
		}
	}
	return b.String(), nil
}

// RewindReport is what a rewind did.
type RewindReport struct {
	Decision  string
	Rewound   []string                      // decision and outcome IDs marked rewound, newest first
	Undone    map[string]*reversible.Result // intent ID -> undo result
	Stranded  []string                      // "intent/effect" that couldn't be undone
	Uncertain []string                      // "intent/effect" whose outcome is unknown
	Errors    []error
}

// Clean reports whether the world is exactly as it was before the decision.
func (r RewindReport) Clean() bool {
	return len(r.Stranded) == 0 && len(r.Uncertain) == 0 && len(r.Errors) == 0
}

// Rewind undoes a decision and everything downstream of it: later
// decisions that built on it, their executions, and outcomes attributed to
// them. Intents are undone newest first. Nodes are marked rewound, never
// deleted, so the ledger keeps the full story, including the mistake.
func (g *Graph) Rewind(ctx context.Context, decisionID string, u Undoer, reason, actor string) (RewindReport, error) {
	g.mu.Lock()
	n, ok := g.nodes[decisionID]
	if !ok {
		g.mu.Unlock()
		return RewindReport{}, fmt.Errorf("unknown node %q", decisionID)
	}
	if n.Kind != KindDecision {
		g.mu.Unlock()
		return RewindReport{}, fmt.Errorf("%q is a %s; only decisions can be rewound", decisionID, n.Kind)
	}
	nodes := g.inOrder(g.downstreamSet(decisionID))
	g.mu.Unlock()

	rep := RewindReport{Decision: decisionID, Undone: map[string]*reversible.Result{}}
	for i := len(nodes) - 1; i >= 0; i-- {
		node := nodes[i]
		if node.Rewound() {
			continue
		}
		for j := len(node.Executions) - 1; j >= 0; j-- {
			intentID := node.Executions[j]
			res, err := u.Undo(ctx, intentID, fmt.Sprintf("rewind of %s: %s", decisionID, reason))
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Errorf("undo %s: %w", intentID, err))
				continue
			}
			rep.Undone[intentID] = res
			for _, e := range res.Stranded() {
				rep.Stranded = append(rep.Stranded, intentID+"/"+e)
			}
			for _, e := range res.Uncertain() {
				rep.Uncertain = append(rep.Uncertain, intentID+"/"+e)
			}
		}
		if err := g.write(Record{Type: RecordRewind, NodeID: node.ID, Actor: actor, Reason: reason, RewindOf: decisionID}); err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Rewound = append(rep.Rewound, node.ID)
	}
	return rep, errors.Join(rep.Errors...)
}

// --- internals ---

func (g *Graph) add(n Node) (*Node, error) {
	if n.ID == "" {
		return nil, errors.New("node has no ID")
	}
	if err := g.write(Record{Type: RecordNode, Node: &n}); err != nil {
		return nil, err
	}
	got, _ := g.Get(n.ID)
	return &got, nil
}

// write validates a record against the current graph, appends it to the
// log, and only then applies it, so the log never holds a record that
// can't be replayed.
func (g *Graph) write(r Record) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	r.Time = g.now()
	if r.Node != nil {
		r.Node.Time = r.Time
	}
	if err := g.check(r); err != nil {
		return err
	}
	// Every record names who is accountable for it.
	switch r.Type {
	case RecordNode:
		r.Actor = r.Node.Actor
	case RecordExecution:
		if r.Actor == "" {
			r.Actor = g.nodes[r.NodeID].Actor
		}
	}
	if err := g.log.Append(r); err != nil {
		return err
	}
	if err := g.applyLocked(r); err != nil {
		return err
	}
	publish(r)
	return nil
}

var mRecords = telemetry.Default.Counter("reversible_ledger_records_total", "Decision ledger records by type: goal, decision, outcome, execution, rewind.", "type", "actor")

// publish reports a ledger record: the history of decisions, as it's
// written, for anyone watching from outside.
func publish(r Record) {
	f := telemetry.F{"actor": r.Actor}
	typ := string(r.Type)
	switch r.Type {
	case RecordNode:
		typ = string(r.Node.Kind)
		f["id"], f["summary"] = r.Node.ID, r.Node.Summary
		if len(r.Node.Parents) > 0 {
			f["parents"] = r.Node.Parents
		}
	case RecordExecution:
		f["decision"], f["intent"] = r.NodeID, r.IntentID
	case RecordRewind:
		f["id"], f["rewind_of"], f["reason"] = r.NodeID, r.RewindOf, r.Reason
	}
	if r.Seq > 0 {
		f["seq"], f["hash"] = r.Seq, telemetry.Short(r.Hash)
	}
	mRecords.Inc(typ, r.Actor)
	telemetry.Emit("ledger."+typ, f)
}

func (g *Graph) check(r Record) error {
	switch r.Type {
	case RecordNode:
		if _, dup := g.nodes[r.Node.ID]; dup {
			return fmt.Errorf("node %q already exists", r.Node.ID)
		}
		for _, p := range r.Node.Parents {
			parent, ok := g.nodes[p]
			if !ok {
				return fmt.Errorf("node %q: unknown parent %q", r.Node.ID, p)
			}
			if parent.Kind == KindOutcome {
				return fmt.Errorf("node %q: parent %q is an outcome; outcomes are observations, not causes", r.Node.ID, p)
			}
			if parent.Rewound() {
				return fmt.Errorf("node %q: parent %q has been rewound", r.Node.ID, p)
			}
		}
	case RecordExecution:
		n, ok := g.nodes[r.NodeID]
		if !ok {
			return fmt.Errorf("unknown decision %q", r.NodeID)
		}
		if n.Kind != KindDecision {
			return fmt.Errorf("%q is a %s; only decisions execute", r.NodeID, n.Kind)
		}
		if n.Rewound() {
			return fmt.Errorf("decision %q has been rewound", r.NodeID)
		}
	case RecordRewind:
		if _, ok := g.nodes[r.NodeID]; !ok {
			return fmt.Errorf("unknown node %q", r.NodeID)
		}
	default:
		return fmt.Errorf("unknown record type %q", r.Type)
	}
	return nil
}

func (g *Graph) apply(r Record) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.check(r); err != nil {
		return err
	}
	return g.applyLocked(r)
}

func (g *Graph) applyLocked(r Record) error {
	switch r.Type {
	case RecordNode:
		n := *r.Node
		g.nodes[n.ID] = &n
		g.order = append(g.order, n.ID)
		for _, p := range n.Parents {
			g.children[p] = append(g.children[p], n.ID)
		}
	case RecordExecution:
		n := g.nodes[r.NodeID]
		n.Executions = append(n.Executions, r.IntentID)
	case RecordRewind:
		n := g.nodes[r.NodeID]
		t := r.Time
		n.RewoundAt, n.RewoundBy, n.RewindReason, n.RewindOf = &t, r.Actor, r.Reason, r.RewindOf
	}
	return nil
}

func (g *Graph) downstreamSet(id string) map[string]bool {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, c := range g.children[id] {
			walk(c)
		}
	}
	walk(id)
	return seen
}

func (g *Graph) inOrder(set map[string]bool) []Node {
	var out []Node
	for _, id := range g.order {
		if set[id] {
			out = append(out, *g.nodes[id])
		}
	}
	return out
}

func arrow(i int) string {
	if i == 0 {
		return "●"
	}
	return "└▶"
}

func formatMetrics(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%g", k, m[k])
	}
	return strings.Join(parts, ", ")
}
