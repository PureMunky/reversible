# Principles

## The root idea

**Change the way software is built.**

Today, software is built by people writing code, and everything else (tickets, reviews, environments, releases) exists to manage changes to that code. In an agentic world, writing code is cheap. What's scarce is knowing exactly what's wanted, and trusting that what was built matches it.

So the thing being built should be *intent*, and code should be what falls out of it:

- **Intent is the source of truth.** People describe what they want, mostly in conversation. The system turns that into a precise spec and asks questions until it is specific enough. Nobody hand-edits the result.
- **Code is a cache.** Implementations are generated from intent, and regenerated whenever the intent, the model or the constraints change. Nobody maintains generated code, any more than they maintain compiler output.
- **History is a record of decisions, not diffs.** Every change traces to the conversation, the person and the reasoning that caused it. You rewind a *decision*, and everything downstream of it is undone.
- **Reversibility replaces permission.** Every effect on the world is declared and undoable by construction. Humans are only asked about the few things that truly can't be undone, and the design keeps that set small.
- **Behaviour is reviewed, not code.** A change is judged by replaying real traffic through the old and new versions and showing what would behave differently, checked against the spec's rules. Product signs off on the reality, not a pull request.
- **Releases become evolution.** New versions run alongside the current one; real outcomes, with statistical evidence, decide which survives. Harm is rewound automatically.
- **Production is an input.** Outcomes and incidents flow back as questions or new intent, so the system corrects itself, even when the fix needs new code.
- **People own outcomes, not code.** The human role is setting intent, defining what must never happen, judging trade-offs and owning taste.

## The framing check

Everything in this repo (every package, demo, doc and commit) should be able to answer:

1. **Which part of the root idea does this serve?** Name the principle above.
2. **Does it treat the software itself as the thing being built?** Or does it only govern something that a person still writes by hand?
3. **Is it domain-neutral?** Commerce, infrastructure, internal tools, content: the mechanism must not assume one. If a demo uses one domain, say it's an example.
4. **Where does a human sit, and why there?** Only at intent, sign-off, trade-offs or one-way doors.

If a change can't answer these, it is plumbing for some other idea.

## Where the repo stands against the root idea

| Principle | Built | Package |
|---|---|---|
| Intent is the source of truth | Conversations become a service spec; vague or contradictory intent comes back as questions and isn't built | `factory`, `intent` |
| Code is a cache | A real service is generated from the spec, built and run as its own process, marked do-not-edit, keyed by the intent's hash and only regenerated when intent changes; a model coder repairs its own failures | `factory` |
| History is decisions, not diffs | Every release traces to the conversation that caused it, in a signed, hash-chained ledger; rewinding a decision takes the running software back | `decision`, `factory` |
| Reversibility replaces permission | Releases, and every other effect, are declared and undoable; crash recovery | `reversible`, `factory` |
| Behaviour is reviewed, not code | Recorded traffic replayed through both versions; every difference traced to the intent change that explains it (including knock-on effects); existing production data read through both versions; unexplained differences block sign-off | `factory`, `counterfactual` |
| Releases become evolution | Gated challengers, significance testing, automatic rewind | `evolve` |
| Production is an input | Recorded traffic and live data judge every change; outcomes calibrate models and drive the next proposals | `factory`, `counterfactual`, `synth` |

**First slice closed:** `go run ./cmd/factory` builds, changes, releases and rewinds a real service from conversation, with no person writing or editing code. Run it with `-faulty-coder` to watch a build that passes every example get stopped because it would silently change existing production data.

**What's still open:**

- **The spec language is small.** One resource, field validation, one kind of rule. Real services need several resources, relations, queries, auth and integrations.
- **The offline coder is a compiler, not a model.** Without `ANTHROPIC_API_KEY`, the spec is written alongside each conversation and code comes from a deterministic generator. The Claude path (capture and coding, with a repair loop) is tested against scripted replies, not yet against a live model.
- **Releases are all at once.** The factory's versions aren't yet released through `evolve`'s gated challengers and live statistics.
- **Production doesn't yet write intent.** Incidents and outcomes are inputs to judging changes, not yet turned into proposed intent changes or questions.
- **Replay assumes a known starting state.** Recorded traffic is replayed from empty; snapshots would remove that limit.
