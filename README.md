# reversible

**Change the way software is built.**

In an agentic world, writing code is cheap. What's scarce is knowing exactly what's wanted, and trusting that what was built matches it. So the thing being built should be *intent*, and code should be a cache that falls out of it: generated, regenerated, never hand-maintained. History should record *decisions*, not diffs. Every effect on the world should be undoable, so agents need undo rather than permission. And changes should be judged by their *behaviour* on real traffic, not by reading code. The full thesis, and how each piece of this repo serves it, is in [PRINCIPLES.md](PRINCIPLES.md).

`reversible` is a working prototype of that idea's foundations, in Go with no dependencies. Every action an agent takes is *declared up front* and *undoable by construction*; every decision is *traceable and rewindable*; every proposed change is *replayed against real traffic* and evolved against the current version on evidence. Only the actions that truly can't be undone reach a person.

**Where it stands:** the first end-to-end slice of the root idea works. A conversation becomes intent, a real service is generated from it, built, verified, diffed by behaviour against production traffic and data, signed off, released and rewound, with no person writing or editing code (`cmd/factory`). Underneath are the foundations: reversible effects, a signed decision ledger, replay, simulation, evolution and synthesis. Other demos use pricing as an example domain; nothing in the mechanism assumes it. What's still open is listed in [PRINCIPLES.md](PRINCIPLES.md#where-the-repo-stands-against-the-root-idea).

## The root idea, end to end

```sh
go run ./cmd/factory                        # offline: deterministic coder
ANTHROPIC_API_KEY=sk-... go run ./cmd/factory  # Claude drafts the spec and writes the code
go run ./cmd/factory -faulty-coder          # a coder with a migration bug, caught before release
go run ./cmd/factory -brittle-coder         # a defect only real traffic shows, stopped by the canary
go run ./cmd/factory -show-prompts          # every prompt a model would be sent, calling nothing
go run ./cmd/factory -keep                  # keep the generated code and data to read afterwards
```

Before running with a real key, read [docs/RUNNING-WITH-CLAUDE.md](docs/RUNNING-WITH-CLAUDE.md): what the model does, what leaves your machine, what runs on it (model-written code is executed, so use a container), and a step-by-step ladder from no risk to a live run.

A real service built, changed, released and rewound from conversation. Nobody writes or edits code.

1. **Phil describes a feature-flag service** for his platform team. The first draft of the spec is checked before anything is built, and the factory asks instead of guessing: an invalid key pattern and an optional key are blocking; a missing owner and no examples of list or delete are clarifying questions.
2. **The factory generates the service** (241 lines of Go), builds it, runs it as its own process, and checks it against the spec's examples: 9/9 pass. Phil signs off on that behaviour; the release is recorded as a decision and goes live.
3. **The team uses it for a week.** Every request is recorded.
4. **Phil changes the intent, not the code:** gradual rollouts, and no deleting a flag that's still on. A new service is generated (12/12 examples pass).
5. **What would change, shown as behaviour.** The week's traffic is replayed through both versions: 17 requests would behave differently, and every one is traced to what Phil asked for (14 by the new field, 1 by the new rule, 2 as knock-on effects of that refused delete). Every existing flag is also read through both versions, to catch anything that misreads old data.
6. **Phil signs off; v2 is released as a canary.** Before it touches production, the review also runs both versions on the same data to check neither destroys the other's fields. Then v2 serves 20% of real traffic beside v1, every outcome is attributed to the version that served it, and after 81 requests with no harm the evolution runtime promotes it.
7. **A team's cleanup script breaks** on the new rule. Phil rewinds the *decision*; v1 is live again, with the data intact.
8. **Production raises questions.** After another week, the factory compares everything v2 did with what v1 would have done from the same data. Two findings: the web team and the nightly cleanup bot each had deletes refused by the new rule. Each becomes a question for Phil, with intent changes as answers: keep refusing, exempt the caller, or drop the rule.
9. **The answers are the new intent.** Phil keeps the rule for people and exempts the bot. v3 is generated, with four of its examples captured from the production requests, so the decision is now contract. Every behaviour change is traced to the exemption; a week later, no new findings.
10. **Every decision stays reversible.** Phil rewinds his answer and v2 is live again on the same data; reaffirming it is served from the cache, with nothing regenerated.

With `-brittle-coder`, the v2 build fails reads of any flag with a description. No example reads one and the review sees none, so it passes everything before release; in the canary it fails real requests, the error guardrail trips after 200 requests (5 of 221 requests ever failed, all on the canary's share), the decision behind it is rewound and v1 serves everything.

With `-faulty-coder`, the v2 build still passes all 12 examples, but reading production data through it shows every existing flag that was on silently dropping to 0% rollout. That change isn't explained by anything Phil asked for, so it isn't released. Examples alone would have shipped it.

## Layers

| Layer | Package | Answers | Key operation |
|---|---|---|---|
| **Factory** | `reversible/factory` | *Build the software itself from intent, release it on evidence, and let production revise it* | `Capture`, `Check`, `Build`, `ReviewCandidate`, `StartCanary`, `Observe`, `Propose` |
| **Effect system** | `reversible` | *What happened, and can it be undone?* | `Execute`, `Undo`, `Release`, `Recover` |
| **Decision ledger** | `reversible/decision` | *Why did it happen, and who decided?* | `Decide`, `Explain`, `Rewind`, `ChainLog` |
| **Counterfactual runtime** | `reversible/counterfactual` | *What would this change do, and what would happen?* | `Replay`, `ReplayWith` + `Simulators` |
| **Intent compiler** | `reversible/intent` | *What does the human actually want?* | `Parse`, `Compile` → checks or questions |
| **Evolutionary runtime** | `reversible/evolve` | *Which version is actually better, with evidence?* | `Propose`, `Serve`, `Observe`, `Evaluate` |
| **Synthesis loop** | `reversible/synth` | *What should we try next?* | `Generator`, `ParamSearch`, `ModelGenerator`, `Loop.Step` |
| **Policy language** | `reversible/policy` | *How can a model write behaviour safely?* | `Parse`, `Validate`, `Compile` |
| **Model client** | `reversible/model` | *How do we talk to the model?* | `Completer`, `Anthropic`, `ExtractJSON` |

```
goal ─▶ decision ─▶ decision ─▶ intent ─▶ effects ─▶ world
           │                        ▲
           └──── rewind ────────────┘  (undo everything downstream, newest first)

recorded traffic ─▶ baseline program  ─┐
                 └▶ candidate program ─┴▶ diff + invariants ─▶ promote / block
                    (every effect intercepted; nothing touches the world)
```

See all three working together on one story:

```sh
go run ./cmd/storyline
```

A pricing agent decides to move customers to annual plans and acts on it. Phil asks *why* one customer ended up on a credit plan and gets the full chain of reasoning. Phil rewinds the decision: five intents across two decisions are undone, newest first, and the one email that can't be unsent is flagged. The agent then proposes v2 pricing, which replay blocks (a new one-way door and a broken invariant); v3 passes.

And the second layer, from human intent to self-correcting production:

```sh
go run ./cmd/evolution
```

Phil writes an intent spec; the compiler sends back questions instead of guessing (a mistyped effect type, impossible bounds, a metric nothing predicts). The fixed spec gates five variants proposed by a pricing agent: two break the spec's rules, one is predicted to drive churn up too far. Two get live traffic. **$65**, which the simulator thought was safe, breaches the churn guardrail; its decision is rewound automatically and its customers are restored before any charge is released. **$52** looks *worse* at first, so it keeps running instead of being judged on luck; after 11,000 more customers it's promoted at p = 0.0001. Meanwhile the simulator calibrates itself against every outcome, and when the agent tries $65 again, the calibrated gate rejects it. Every step is in a signed, hash-chained ledger that detects a rewritten rationale.

And with no human in the loop after the spec is written:

```sh
go run ./cmd/autopilot
```

```
round  1  [   550 customers]  champion $55   propose $52; propose $58
round  2  [  1050 customers]  champion $52   PROMOTE $52 over $55 (+7.4%, p=0.00032)
round  3  [  1550 customers]  champion $52   propose $49
round  5  [  2550 customers]  champion $52   retire $58 (worse on objective)
round 16  [  8050 customers]  champion $52   retire $49 (inconclusive)
round 17  converged: no untried neighbour of $52 is left, nothing is running
```

The synthesis loop searches prices inside the spec's approved range, one step from the champion at a time. The true revenue-maximising price ($50, which nothing in the system is told) lies between its grid points; it settles on $52, and correctly calls $49 a statistical tie (the true gap is under 1%).

And with a model designing whole policies, not just prices:

```sh
go run ./cmd/designer                          # offline stand-in
ANTHROPIC_API_KEY=sk-... go run ./cmd/designer # Claude designs the policy
go run ./cmd/designer -show-prompt             # see exactly what the model is told
```

Customers' willingness to pay depends on how long they've subscribed, which nobody tells the system. Starting from a flat $55, the designer proposes splitting price by tenure, then refines each segment; one rule-breaking idea ($120 for everyone) is stopped by the gate before a customer sees it. Measured on 40,000 customers the system never saw:

| Policy | Revenue per customer |
|---|---|
| Starting policy, flat $55 | $25.14 |
| Best possible single price ($42), the ceiling for any price-only search | $35.96 |
| **Designed policy** (new $35, mid $45, loyal $70) | **$42.37** |
| True per-segment optimum ($35/$45/$60) | $43.86 |

Without an API key the demo uses an offline stand-in: a deterministic heuristic that reads the same prompt and replies in the same JSON, so the demo is reproducible. It is labelled as such every time it runs.

## The effect system

Every action is an **Effect** with one of three classes:

| Class | When it runs | How it's undone |
|---|---|---|
| `Reversible` | Immediately | A required compensating `Undo` (delete what you created, restore what you changed) |
| `Escrowed` | Held, then applied on `Release` or when its hold window expires | Before release: cancelled for free. After release: optional `Undo` (e.g. a refund) |
| `Irreversible` | Only after a `Gate` approves it | Can't be. Reported as *stranded* if the intent is undone later |

Effects are grouped into an **Intent**: a goal plus every effect needed to achieve it. The runtime enforces a few rules:

- **Declare first.** All effects are known and validated before anything runs.
- **Approve one-way doors before the first step.** Every irreversible effect is approved during preflight, so a denial never leaves the world half-changed.
- **One-way doors go last.** Irreversible effects must come after everything else, so a failure can still roll back everything before them.
- **Failure rolls back automatically.** If any effect fails, everything already done is compensated in reverse order, and held escrow is cancelled.
- **Deny by default.** The default gate is `DenyAll`, so an unattended agent can never take an irreversible action by accident.
- **Everything is on the ledger.** Every declaration, approval, execution, hold, release, cancellation and compensation is an append-only event, with the goal and receipts attached.

## Quick start

```go
rt := reversible.New(
    reversible.WithGate(myApprover),          // default: DenyAll
    reversible.WithHold(10*time.Minute),      // escrow window
    reversible.WithLedger(reversible.NewFileLedger("ledger.jsonl")),
)

res, err := rt.Execute(ctx, reversible.Intent{
    ID:   "onboard-acme",
    Goal: "onboard Acme on the $49 plan",
    Effects: []reversible.Effect{
        createAccount("acme"),   // Reversible
        charge("acme", 49),      // Escrowed
        sendWelcome("acme"),     // Escrowed
    },
})
// res.Status == pending: account exists, charge and email are held

rt.Undo(ctx, "onboard-acme", "wrong plan")  // as if it never happened
// or
rt.Release(ctx, "onboard-acme")             // apply the held effects now
// or, on a ticker
rt.ReleaseDue(ctx)                          // apply escrow whose window has passed
```

Defining an effect:

```go
func createAccount(name string) reversible.Effect {
    return reversible.Effect{
        Name:  "create-account",
        Class: reversible.Reversible,
        Do: func(ctx context.Context) (reversible.Receipt, error) {
            id, err := db.CreateAccount(ctx, name)
            return reversible.Receipt{"account": id}, err
        },
        Undo: func(ctx context.Context, r reversible.Receipt) error {
            return db.DeleteAccount(ctx, r["account"])
        },
    }
}
```

## Durability and crash recovery

Closures can't be written to disk, so durable effects are built from a **Registry** of named effect types. An effect becomes a serializable **Spec** (`type` + `params`), and any process with the same registry can rebuild it.

```go
reg := reversible.NewRegistry()
reg.MustRegister("card.charge", reversible.Handler{
    Class: reversible.Escrowed,
    Do: func(ctx context.Context, p reversible.Params) (reversible.Receipt, error) {
        id, err := payments.Charge(ctx, p["customer"], p["amount"], reversible.IdempotencyKey(ctx))
        return reversible.Receipt{"charge": id}, err
    },
    Undo: func(ctx context.Context, p reversible.Params, r reversible.Receipt) error {
        return payments.Refund(ctx, r["charge"])
    },
    // Did a charge with this idempotency key go through before we crashed?
    Reconcile: func(ctx context.Context, p reversible.Params) (bool, reversible.Receipt, error) {
        id, found, err := payments.FindByKey(ctx, reversible.IdempotencyKey(ctx))
        return found, reversible.Receipt{"charge": id}, err
    },
})

store, _ := reversible.NewFileStore("state/intents")
rt := reversible.New(reversible.WithStore(store), reversible.WithRegistry(reg))
rt.Recover(ctx) // always first, on every startup

rt.Execute(ctx, reversible.Intent{ID: "bill-acme", Effects: []reversible.Effect{
    reg.MustEffect("charge", "card.charge", reversible.Params{"customer": "acme", "amount": "49"}),
}})
```

With a store attached:

- **Write-ahead.** Each step is saved as `in_flight` *before* its `Do` runs. If that save fails, the effect doesn't run and the intent rolls back.
- **Escrow survives restarts.** Held effects, receipts and release times are on disk. A new process recovers them and can release or undo them.
- **A crash counts as a failure.** If a process dies mid-intent, the next `Recover` rolls back everything that finished. Escrow after the crash point never runs.
- **The world settles uncertainty, not a guess.** The step that was mid-flight is marked **uncertain**. If its type has a `Reconcile` hook, recovery asks the world whether it happened: if it did, it's compensated in order with everything else; if not, it's cancelled. Without a hook (or if the world is unreachable), it stays uncertain and is flagged, and `rt.Reconcile(ctx, id)` retries later.
- **Stable idempotency keys.** Every `Do`, `Undo` and `Reconcile` call can read `reversible.IdempotencyKey(ctx)` (`<intent>/<effect>`), which is identical across retries and restarts. Pass it to downstream APIs to dedupe requests, and use it in `Reconcile` to look the effect up.
- **No double compensation.** State is saved after each compensation, and recovering an already-recovered intent is a no-op.
- **One owner at a time.** A `FileStore` is locked exclusively by the first runtime to `Execute` or `Recover`; a second process gets `ErrLocked`. `Close` (or process exit) releases it.
- **Durable means durable.** A runtime with a store rejects closure-only effects (`ErrNotDurable`) instead of quietly losing them on restart.

Handlers should be idempotent, since recovery and retries may call them for an effect whose outcome is unknown.

### Try it across real process restarts

`agentctl` is a CLI where every command is a separate process, backed by a fake world on disk:

```sh
go run ./cmd/agentctl -hold 30s onboard acme   # account + workspace created; charge + email held
go run ./cmd/agentctl status
go run ./cmd/agentctl undo onboard-acme wrong plan
go run ./cmd/agentctl onboard globex 99
go run ./cmd/agentctl release onboard-globex   # or wait out the hold and run: tick
go run ./cmd/agentctl crash initech            # dies after creating a workspace it never recorded (exit 137)
go run ./cmd/agentctl world                    # next process reconciles, finds the orphan, rolls it all back
go run ./cmd/agentctl log onboard-initech      # the ledger tells the story
```

## The decision ledger

The effect runtime records *what* happened. `decision` records *why*, as an append-only causal graph:

```go
g, _ := decision.Open(decision.NewFileLog("decisions.jsonl"))

g.Goal("grow-revenue", "grow revenue 10% this quarter", "phil")
g.Decide(decision.Decision{
    ID: "annual-plans", Parents: []string{"grow-revenue"}, Actor: "pricing-agent",
    Summary:      "move monthly customers to annual plans",
    Rationale:    "annual billing pulls revenue forward",
    Alternatives: []string{"raise monthly prices 8%", "upsell add-ons"},
})
g.Execute(ctx, rt, "annual-plans", intent)   // links the intent, then runs it
g.Observe(decision.Outcome{ID: "week-1", Parents: []string{"annual-plans"},
    Summary: "support tickets spiked", Metrics: map[string]float64{"tickets": 41}})

why, _ := g.ExplainIntent("annual-acme")     // goal → decisions → rationale → alternatives
rep, _ := g.Rewind(ctx, "annual-plans", rt, "support load too high", "phil")
```

- **Every decision traces to a goal.** Decisions must have parents; outcomes can't be causes; nothing can build on a rewound decision.
- **Links before actions.** `Execute` records which decision an intent belongs to *before* running it, so nothing in the world is ever unexplained.
- **Rewind a decision, not an action.** `Rewind` finds the decision and everything downstream of it (later decisions that built on it, their intents, outcomes attributed to them) and undoes the intents newest first through the effect runtime. The report lists what was undone, and anything stranded or uncertain.
- **Nothing is deleted.** Rewound nodes are marked with who, when, why and as part of which rewind. The mistake stays in the story.
- **Replayable.** The graph is rebuilt from its log on `Open`; every record is validated before it's written, so the log never holds something that can't be replayed.

## The counterfactual runtime

Agent behaviour is modelled as a `Program`: given a request, it returns the `Intent` it *would* execute. That makes behaviour testable without touching anything.

```go
traffic := counterfactual.NewFileRecorder("traffic.jsonl")
live := counterfactual.Recording(v1, traffic)          // record real requests as they happen

reqs, _ := traffic.Requests()
rep := counterfactual.Replay(ctx, reqs, v1, v2,
    []counterfactual.Invariant{maxCharge},              // rules every plan must satisfy
    counterfactual.Policy{MaxChangedFraction: 0.25})    // blast-radius limit
fmt.Print(rep)                                          // or rep.JSON() for CI / another agent
if rep.Promote() { /* ship v2 */ }
```

For every recorded request, baseline and candidate both run with every effect intercepted (`Do` is never called), and the report shows exactly what would have changed:

```
replayed 4 requests: 2 unchanged, 2 changed
  req-3 (active=false customer=initech tier=enterprise)
    + purge-inactive [account.purge] {customer=initech}
    ~ charge [card.charge] {amount=4500, ...}  →  charge [card.charge] {amount=6500, ...}
    ⚠ new one-way door: purge-inactive
    ✗ violates charge-at-most-5000: charges $6500
verdict: BLOCKED
  - candidate violates invariants on 1 request(s)
  - candidate introduces irreversible effects on 2 request(s)
```

A candidate is blocked when it fails, panics, or produces an intent the runtime would reject on requests the baseline handles; when it *newly* violates an invariant (violations the baseline already had are shown but don't block); when it opens one-way doors the baseline never did (unless the policy allows it); or when it changes more traffic than the blast-radius limit.

### Predicting consequences: world simulators

Replay compares *plans*. Simulators add a model of how the world responds, so the report can show predicted *consequences* and guardrails can act on them:

```go
rep := counterfactual.ReplayWith(ctx, counterfactual.Config{
    Requests: reqs, Baseline: v1, Candidate: v2, Invariants: invs,
    Simulators: counterfactual.Simulators{
        "card.charge": func(req counterfactual.Request, e counterfactual.PlannedEffect) map[string]float64 {
            price, _ := strconv.ParseFloat(e.Params["amount"], 64)
            churn := churnModel(req, price)
            return map[string]float64{"revenue": price * (1 - churn), "churn": churn}
        },
    },
    Guardrails: []counterfactual.Guardrail{
        counterfactual.NoDropOver("revenue", 5),
        counterfactual.NoRiseOver("churn", 10),
    },
})
```

The report gains per-request deltas (`Δ churn +0.15, revenue +25.3`) and predicted totals, and a breached guardrail blocks promotion. Simulators are models and will be wrong; the evolutionary runtime is what checks them against reality.

## The intent compiler

A person states what they want in a spec, and `intent.Compile` turns it into invariants, guardrails and policy:

```json
{
  "goal": "maximise subscription revenue without driving customers away",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "max": 5000, "why": "finance signs off above $5k"},
    {"type": "account.purge", "forbid": true, "why": "legal hold on customer data"},
    {"type": "card.charge", "requires": "email.send", "why": "every charge gets a receipt"}
  ],
  "guardrails": [
    {"metric": "revenue", "no_drop_over_pct": 10},
    {"metric": "churn", "no_rise_over_pct": 25}
  ],
  "policy": {"max_changed_pct": 50}
}
```

```go
spec, _ := intent.Load("intent.json")
c := intent.Compile(spec, intent.Known{Types: reg.Types(), Metrics: []string{"revenue", "churn"}})
if !c.Ready() {
    for _, q := range c.Questions { fmt.Println(q) }   // ask the human; don't guess
}
rep := counterfactual.ReplayWith(ctx, c.Config(counterfactual.Config{Requests: reqs, Baseline: v1, Candidate: v2}))
```

Rules are `forbid`, `min`/`max` on a param, `requires`, `at_most` and `class`; the human's `why` is carried into every violation. The compiler doesn't guess: unknown fields are rejected, and mistyped types or metrics (`did you mean "card.charge"?`), impossible or conflicting bounds, requirements on forbidden types, rules mixing constraints, class mismatches with the registry and missing goals come back as **blocking** questions. Softer issues (no owner, nothing constrained, new one-way doors allowed) come back as **clarify**.

## The evolutionary runtime

`evolve.Population` runs a champion and challengers side by side, and lets outcomes decide:

```go
pop, _ := evolve.New(evolve.Variant{Name: "$55", Program: v55, Decision: "price-55"}, evolve.Options{
    Gate:            compiled.Config(counterfactual.Config{Simulators: sims}), // every challenger must pass this
    Recorder:        counterfactual.NewMemoryRecorder(),
    ChallengerShare: 0.2,
    Objective:       evolve.Objective{Metric: "revenue", Maximize: true, MinLiftPct: 2},
    Guardrails:      compiled.Guardrails,                                       // checked on observed outcomes
    MinSamples:      60,
    Ledger: g, AutoRewind: true, Undoer: rt, Actor: "evolve",
})

pop.Propose(ctx, evolve.Variant{Name: "$52", Program: v52, Decision: "try-52"}) // replay-gated
pop.Serve(ctx, rt, req)                // stable hash routing; intent linked to the variant's decision
pop.Observe(req.ID, map[string]float64{"revenue": 52, "churn": 0})
verdict, _ := pop.Evaluate(ctx)        // promote, retire (+ auto-rewind), or wait
```

- **No traffic without the gate.** A challenger is replayed against recorded traffic under the same invariants, simulators, guardrails and policy as any change. Rejected variants never serve.
- **Stable routing.** Requests are assigned by a hash of their ID, so retries land on the same variant.
- **Outcomes, not predictions, decide.** A challenger that beats the champion's average on the objective by `MinLiftPct` (with `MinSamples` on both sides) becomes champion. One whose observed averages breach a guardrail is retired.
- **Outcome-aware rewind.** Each evaluation is recorded as an outcome on the variant's decision. With `AutoRewind`, a retired challenger's decision is rewound, undoing every intent it served; without it, the verdict suggests the rewind.

## Accountability: a signed, hash-chained ledger

Wrap any decision log in a `ChainLog` and every record is linked to the previous one by hash and signed (ed25519) by the actor accountable for it:

```go
keys := decision.NewKeyring()
keys.Generate("phil"); keys.Generate("pricing-agent")
keys.Default = "phil"                                   // signs records with no actor (e.g. outcomes)

chain, _ := decision.NewChainLog(decision.NewFileLog("decisions.jsonl"), keys)
g, _ := decision.Open(chain)

// Later, anyone with only the public keys:
_, err := decision.NewChainLog(decision.NewFileLog("decisions.jsonl"), keys.PublicKeys())
// errors.Is(err, decision.ErrChain) if anything was altered
```

An actor with no signing key can't record a decision at all. Verification catches edited content, deleted or reordered records, re-attribution to another actor, and signatures forged with the wrong key, and `NewChainLog` refuses to extend a broken chain.

### Calibration: simulators learn from reality

A `Calibrator` pairs what the simulators predicted for each served request with what was then observed, and fits a per-metric linear correction. The gate then uses corrected predictions:

```go
cal := counterfactual.NewCalibrator()
cal.Bounds = map[string][2]float64{"churn": {0, 1}}
pop, _ := evolve.New(champion, evolve.Options{Gate: gate, Calibrator: cal /* ... */})
// every Serve records a prediction; every Observe pairs it with reality;
// every Propose gates with cal.Fit()
fmt.Print(pop.Calibration())
// churn    n=12680  observed ≈ -0.299 + 7.28 × predicted, error (RMSE) 0.287 → 0.284
```

Two safeguards keep it from learning noise, both found the hard way (an early version fitted a slope to single-price data, extrapolated it, and rejected every proposal, locking the system into the status quo):

- **A learned slope must be real.** It's only used if statistically distinguishable from 1 (|t| ≥ 2). Otherwise only a level shift is applied. Data from one operating point can say the model is off, not how it responds to change.
- **No extrapolation.** Outside the range of predictions it was fitted on, the edge correction is held and the raw model's slope takes over.

### Sound evaluation

`Evaluate` decides on evidence, not averages:

- **Welch's t-test** on every comparison (running mean and variance per metric per variant).
- **Alpha spending.** Each challenger's α is spent across evaluations as 6α/(π²k²), which sums to α. Evaluating after every batch can't inflate false positives, and a real effect can still reach significance after many looks.
- **Separate budgets.** Promotions use `Alpha` (default 0.05). Guardrail breaches use the looser `GuardrailAlpha` (default 0.10), because stopping harm early matters more than certainty. A suspected but unconfirmed breach blocks promotion.
- **Four outcomes.** Promote (significantly better by `MinLiftPct`). Retire for a **guardrail breach** (rewound with `AutoRewind`). Retire as **worse** (its actions were within bounds, so they stand). Retire as **inconclusive** at `MaxSamples`.
- **Unbiased assignment.** Traffic is split by a keyed SHA-256 of the request ID under a per-population salt. An earlier FNV-based split was correlated with another FNV hash of the same ID used in the demo world, which made a worse price look significantly better (p = 0.0026). Any hash of the ID used elsewhere could do the same; a keyed hash can't.

## The synthesis loop

`synth.Loop` proposes variants itself. A `Generator` sees the champion and every trial so far and returns candidates; each one is recorded as a signed decision with its rationale, then handed to the population, which gates, runs, judges and (if needed) rewinds it.

```go
knob := synth.KnobFromSpec(spec, "price", "card.charge", "amount", 3, 0, 1000) // bounds from the spec
loop, _ := synth.New(pop, synth.ParamSearch{
    Knobs:   []synth.Knob{knob},
    Factory: func(p synth.Point) counterfactual.Program { return pricing(int(p["price"])) },
}, ledger, synth.Point{"price": 55}, synth.Options{Goal: "goal", Actor: "synth", MaxLive: 2})

for {
    rep, _ := loop.Step(ctx)   // top up to MaxLive challengers
    if rep.Converged { break } // nothing left to try, nothing running
    serveTraffic()
    pop.Evaluate(ctx)
}
```

`ParamSearch` tries the champion's untried neighbours within the spec's bounds. The `Generator` interface is where a language model plugs in, proposing whole new programs from the compiled spec and the trial history. The safety story doesn't change with the generator: a rogue generator that proposes something outside the spec is stopped by the gate (there's a test for exactly that).

## Models that write policies

`ModelGenerator` lets a language model propose whole behaviours, safely:

```go
gen := &synth.ModelGenerator{
    Model:    anthropicClient,           // or any model.Completer
    Spec:     spec,                      // goal, rules (with their "why"), guardrails
    Registry: reg,                       // which effect types exist
    Examples: someRecordedRequests,      // which inputs exist
}
gen.Register("flat-55", startingPolicy)
loop, _ := synth.New(pop, gen, ledger, nil, synth.Options{Goal: "goal", Actor: "designer"})
```

Every prompt contains the goal and every rule's `why`, the guardrails, the registered effect types, example requests, the policy language, the champion's policy, and every trial so far with its observed outcomes, hypothesis and result. The model replies with JSON.

**The model writes data, not code.** Each proposal is a `policy.Policy`: named variables plus ordered rules whose conditions on request inputs choose the effects to plan.

```json
{
  "vars": {"new": 35, "mid": 45, "loyal": 70},
  "rules": [
    {"when": [{"input": "tenure_months", "op": "<", "value": "6"}],
     "effects": [{"name": "charge", "type": "card.charge", "params": {"customer": "${input.customer}", "amount": "${var.new}"}},
                 {"name": "receipt", "type": "email.send", "params": {"to": "${input.customer}"}}]},
    {"when": [{"input": "tenure_months", "op": ">=", "value": "24"}],
     "effects": [/* ... ${var.loyal} ... */]},
    {"effects": [/* ... ${var.mid} ... */]}
  ]
}
```

A policy can't run code, reach the network or use an effect type that isn't registered. `policy.Validate` reports every problem at once. A proposal that doesn't parse or validate still becomes a candidate whose program fails, so the gate rejects it **on the record**: the ledger keeps what the model tried and why it was refused. Model and parse failures surface as errors, never as "nothing left to try". Every prompt and reply is kept for audit (`gen.Transcript()`).

`model.Anthropic` is a dependency-free client for the Messages API (`model.FromEnv()` reads `ANTHROPIC_API_KEY` and an optional `REVERSIBLE_MODEL`).

### Harm is measured against the launch baseline

A challenger's guardrails compare it with the champion it was **admitted against**: what customers would have got had the experiment never run. Its objective compares it with the **current** champion. So a challenger that was fine when launched, but is outclassed by a later winner, is retired as *worse* (its customers keep the valid price they paid), not rewound as *harmful*. The designer demo found this: before the fix, a policy that beat the original was rewound, refunding 2,759 valid charges, because its churn looked high next to a newer champion.

## Run the in-memory demo

```sh
go run ./cmd/demo            # irreversible effects denied (default)
go run ./cmd/demo -approve   # a human approves the one-way door
```

The demo walks an agent through onboarding customers against a fake world:

1. Onboard Acme: account created, charge and email held.
2. Undo it: no refund needed, no awkward email.
3. Onboard Globex and release the escrow.
4. Undo after release: the charge is refunded, the sent email is reported as stranded.
5. A step fails partway: everything rolls back on its own.
6. Purge legacy data: denied before anything runs, unless `-approve` is passed.

## Statuses

| Status | Meaning |
|---|---|
| `running` | Effects being applied (if found at startup: a crash, which `Recover` rolls back) |
| `pending` | Reversible effects applied, escrowed effects held |
| `committed` | Everything applied |
| `rolled_back` | Undone, and nothing remains in the world |
| `partially_rolled_back` | Undone, but some effects couldn't be (`Result.Stranded()`) or may have happened (`Result.Uncertain()`) |
| `rejected` | A gate denied an irreversible effect, so nothing ran |

## Limitations (it's a prototype)

- **Reconcile is only as good as the downstream system.** It works when the world can be queried by idempotency key. Systems that can't (fire-and-forget email, some legacy APIs) leave steps uncertain.
- **Three logs, not one.** The effect store (source of truth for recovery), the effect ledger (audit trail) and the decision log are separate files. They're linked by intent ID, but not written atomically together.
- **Compensation is trusted.** An `Undo` that lies or partially succeeds isn't detected. Verifying that compensation actually restored the prior state is an open problem.
- **Simulators are only as good as their models.** Predictions are summed per effect, with no interaction effects or uncertainty estimate. Calibration corrects a model's level and (with evidence) its slope on one aggregate prediction; it can't fix a model with the wrong structure.
- **Statistics are frequentist and conservative.** Welch's t-test with a union-bound alpha spend is sound but slow; group-sequential or Bayesian designs would reach decisions with fewer customers. Metrics are tested one at a time, with no correction across guardrails.
- **The search is local.** `ParamSearch` walks a fixed grid one step at a time, so it can stop between grid points (as it does at $52 vs the true $50) or in a local optimum.
- **Programs must be pure.** Replay assumes a `Program` only describes effects. One that performs I/O while deciding will do it during replay too.
- **The live-model path is tested against a mock API, not a live model.** The client, prompt and parsing are covered by tests and the offline stand-in; how well a given Claude model designs policies is for you to try with your key.
- **The policy language is deliberately small.** Conditions are conjunctions of comparisons; there's no arithmetic in params and no state across requests.
- **Keys are in memory.** The keyring has no persistence, rotation or revocation.
- **Single machine.** File locking is advisory and Unix-only; there's no distributed coordination across services.

## Where this is going

The loop is closed: a person writes intent; the system proposes, gates, runs, judges, rewinds and learns, and every step is signed. What's left is making each part stronger:

- **Model-proposed experiments with memory.** Let the model see calibration results and per-segment outcomes, not just averages.
- **A richer policy language.** Arithmetic in params, segment-level metrics, and policies that compose.
- **Adaptive search.** Shrink the step near an optimum, and explore beyond local maxima.
- **Better simulators.** Per-segment calibration, uncertainty estimates, and gates that use the uncertainty.
- **Faster, sound decisions.** Group-sequential or Bayesian evaluation; multiple-testing correction across guardrails.
- **Contract negotiation.** Agents agreeing interfaces at runtime, checked by the same gate.
- **Distributed effects.** Coordinated escrow and rewind across services.

## License

MIT
