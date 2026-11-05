// Package synth closes the loop: it proposes new variants on its own,
// records each proposal as a decision, and hands it to an evolutionary
// population, which gates it, runs it, judges it and rewinds it if needed.
//
// What proposes is pluggable. A Generator sees the current champion and
// every trial so far and returns candidates. ParamSearch, included here,
// searches the neighbourhood of the champion over numeric knobs whose
// bounds come from the intent spec. A Generator backed by a language
// model, proposing whole new programs from the spec and the trial
// history, plugs in the same way; the gate and the population don't care
// where a candidate came from, which is the point.
package synth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
	"github.com/PureMunky/reversible/intent"
)

// Point is a setting of knobs.
type Point map[string]float64

// Key is a stable identity for a point.
func (p Point) Key() string {
	names := make([]string, 0, len(p))
	for k := range p {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + strconv.FormatFloat(p[k], 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}

func (p Point) clone() Point {
	out := Point{}
	for k, v := range p {
		out[k] = v
	}
	return out
}

// Knob is one numeric parameter the search may move.
type Knob struct {
	Name     string
	Min, Max float64
	Step     float64
}

// KnobFromSpec takes a knob's bounds from the spec's min/max rules on the
// given effect type and param, so the search can never propose something
// the spec already forbids. Fallbacks apply where the spec is silent.
func KnobFromSpec(s intent.Spec, name, effectType, param string, step, fallbackMin, fallbackMax float64) Knob {
	k := Knob{Name: name, Min: fallbackMin, Max: fallbackMax, Step: step}
	for _, r := range s.Rules {
		if r.Type != effectType || r.Param != param {
			continue
		}
		if r.Min != nil {
			k.Min = math.Max(k.Min, *r.Min)
		}
		if r.Max != nil {
			k.Max = math.Min(k.Max, *r.Max)
		}
	}
	return k
}

// Candidate is a proposed variant.
type Candidate struct {
	Name      string
	Point     Point // optional, for knob-based generators
	Program   counterfactual.Program
	Rationale string
}

// Trial is what happened to a candidate.
type Trial struct {
	Candidate
	Decision string
	Status   evolve.Status
	Reason   string
}

// State is what a Generator sees.
type State struct {
	Champion      string
	ChampionPoint Point
	Trials        []Trial // every candidate ever proposed, in order
}

// Tried reports whether a point has already been proposed.
func (s State) Tried(p Point) bool {
	k := p.Key()
	for _, t := range s.Trials {
		if t.Point != nil && t.Point.Key() == k {
			return true
		}
	}
	return false
}

// Generator proposes candidates. Returning none means it has nothing left
// to try from this state.
type Generator interface {
	Propose(ctx context.Context, s State) ([]Candidate, error)
}

// GeneratorFunc adapts a function to Generator.
type GeneratorFunc func(ctx context.Context, s State) ([]Candidate, error)

func (f GeneratorFunc) Propose(ctx context.Context, s State) ([]Candidate, error) { return f(ctx, s) }

// ParamSearch proposes the champion's untried neighbours, one step along
// each knob in each direction, within the knobs' bounds.
type ParamSearch struct {
	Knobs   []Knob
	Factory func(Point) counterfactual.Program
	// Name renders a point as a variant name (default: its Key).
	Name func(Point) string
}

func (ps ParamSearch) Propose(_ context.Context, s State) ([]Candidate, error) {
	if ps.Factory == nil {
		return nil, errors.New("ParamSearch needs a Factory")
	}
	var out []Candidate
	for _, k := range ps.Knobs {
		for _, dir := range []float64{-1, 1} {
			p := s.ChampionPoint.clone()
			v := p[k.Name] + dir*k.Step
			if v < k.Min || v > k.Max {
				continue
			}
			p[k.Name] = v
			if s.Tried(p) {
				continue
			}
			name := p.Key()
			if ps.Name != nil {
				name = ps.Name(p)
			}
			verb := "raise"
			if dir < 0 {
				verb = "lower"
			}
			out = append(out, Candidate{
				Name:    name,
				Point:   p,
				Program: ps.Factory(p),
				Rationale: fmt.Sprintf("%s %s by %g from champion %s (bounds %g–%g from the spec)",
					verb, k.Name, k.Step, s.Champion, k.Min, k.Max),
			})
		}
	}
	return out, nil
}

// Options configures a Loop.
type Options struct {
	// Goal is the ledger node every proposal decision hangs from.
	Goal  string
	Actor string // who proposals are attributed to
	// MaxLive caps how many challengers run at once (default 2).
	MaxLive int
}

// Loop repeatedly asks a Generator for candidates and proposes them to a
// Population, recording each as a decision.
type Loop struct {
	mu     sync.Mutex
	pop    *evolve.Population
	gen    Generator
	ledger *decision.Graph
	opts   Options
	points map[string]Point // variant name -> point
	trials []Trial
	seq    int
}

// New creates a loop. championPoint describes the population's current
// champion in knob terms (nil for generators that don't use knobs).
func New(pop *evolve.Population, gen Generator, ledger *decision.Graph, championPoint Point, opts Options) (*Loop, error) {
	if ledger != nil && opts.Goal == "" {
		return nil, errors.New("a ledger needs a Goal to hang decisions from")
	}
	if opts.MaxLive == 0 {
		opts.MaxLive = 2
	}
	l := &Loop{pop: pop, gen: gen, ledger: ledger, opts: opts, points: map[string]Point{}}
	champ := pop.Champion()
	if championPoint != nil {
		l.points[champ.Name] = championPoint.clone()
	}
	// The starting champion counts as tried, so it's never re-proposed.
	l.trials = append(l.trials, Trial{Candidate: Candidate{Name: champ.Name, Point: championPoint, Program: champ.Program,
		Rationale: "starting champion"}, Decision: champ.Decision, Status: evolve.Champion})
	return l, nil
}

// StepReport is what one step proposed.
type StepReport struct {
	Admitted []Trial
	Rejected []Trial
	// Converged is true when nothing is running and the generator has
	// nothing left to propose: the champion is the best this search found.
	Converged bool
}

// State returns the loop's current view, refreshed from the population.
func (l *Loop) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked()
}

func (l *Loop) stateLocked() State {
	status := map[string]evolve.VariantStatus{}
	for _, s := range l.pop.Status() {
		status[s.Name] = s
	}
	for i := range l.trials {
		if s, ok := status[l.trials[i].Name]; ok {
			l.trials[i].Status = s.Status
			if s.Reason != "" {
				l.trials[i].Reason = s.Reason
			}
		}
	}
	champ := l.pop.Champion().Name
	return State{Champion: champ, ChampionPoint: l.points[champ], Trials: append([]Trial(nil), l.trials...)}
}

// Step tops the population up to MaxLive challengers with new candidates.
func (l *Loop) Step(ctx context.Context) (StepReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked()
	live := 0
	for _, t := range st.Trials {
		if t.Status == evolve.Challenger {
			live++
		}
	}
	var rep StepReport
	if live >= l.opts.MaxLive {
		return rep, nil
	}
	cands, err := l.gen.Propose(ctx, st)
	if err != nil {
		return rep, err
	}
	if len(cands) == 0 && live == 0 {
		rep.Converged = true
		return rep, nil
	}
	for _, c := range cands {
		if live >= l.opts.MaxLive {
			break
		}
		if l.known(c) {
			continue // a generator re-proposing something already tried
		}
		l.seq++
		t := Trial{Candidate: c}
		if l.ledger != nil {
			t.Decision = fmt.Sprintf("synth-%d-%s", l.seq, sanitize(c.Name))
			if _, err := l.ledger.Decide(decision.Decision{ID: t.Decision, Parents: []string{l.opts.Goal},
				Summary: "try " + c.Name, Rationale: c.Rationale, Actor: l.opts.Actor}); err != nil {
				return rep, err
			}
		}
		gate, err := l.pop.Propose(ctx, evolve.Variant{Name: c.Name, Program: c.Program, Decision: t.Decision})
		if err != nil {
			return rep, err
		}
		if c.Point != nil {
			l.points[c.Name] = c.Point.clone()
		}
		if gate.Promote() {
			t.Status = evolve.Challenger
			live++
			rep.Admitted = append(rep.Admitted, t)
		} else {
			t.Status, t.Reason = evolve.Rejected, strings.Join(gate.Blockers, "; ")
			rep.Rejected = append(rep.Rejected, t)
		}
		l.trials = append(l.trials, t)
	}
	return rep, nil
}

func (l *Loop) known(c Candidate) bool {
	for _, t := range l.trials {
		if t.Name == c.Name || (c.Point != nil && t.Point != nil && t.Point.Key() == c.Point.Key()) {
			return true
		}
	}
	return false
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, s)
}
