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
| Intent is the source of truth | Conversations become a service spec covering several resources, relations between them, lifecycles (allowed state changes), filters and who may change what; vague or contradictory intent comes back as questions and isn't built. Two unrelated services (feature flags, an incident tracker) come from the same factory with nothing domain-specific in it | `factory`, `intent` |
| Code is a cache | A real service is generated from the spec, built and run as its own process, marked do-not-edit, keyed by the intent's hash and only regenerated when intent changes; a model coder repairs its own failures | `factory` |
| History is decisions, not diffs | Every release traces to the conversation that caused it, in a signed, hash-chained ledger; rewinding a decision takes the running software back | `decision`, `factory` |
| Reversibility replaces permission | Releases, and every other effect, are declared and undoable; crash recovery | `reversible`, `factory` |
| Behaviour is reviewed, not code | Recorded traffic replayed through both versions; every difference traced to the intent change that explains it (including knock-on effects); existing production data read through both versions; unexplained differences block sign-off | `factory`, `counterfactual` |
| Releases become evolution | A reviewed build runs as a canary beside the live version on the same data; every request's outcome is attributed to the version that served it, and significance testing promotes it when there's enough evidence of no harm, or retires it and rewinds the decision behind it when it does harm | `factory`, `evolve` |
| Production is an input | After each release, production traffic is compared with what the previous version would have done from the same data; refusals and failures become questions for the owner whose answers are new intent, and the production request becomes part of the contract | `factory`, `counterfactual`, `synth` |

**First slice closed:** `go run ./cmd/factory` builds, changes, releases and rewinds a real service from conversation, with no person writing or editing code. Run it with `-faulty-coder` to watch a build that passes every example get stopped because it would silently change existing production data. `go run ./cmd/incidents` builds a different kind of software, an incident tracker with related resources and a lifecycle, from the same factory.

**What's still open:**

- **The spec language covers records, not computation.** Resources, references, lifecycles, filters and writers are intent now (`cmd/incidents`). Anything that computes (derived values, aggregates), runs on its own (jobs, timers, escalation), reacts to events, or calls another system still has to be written by hand, so most real software can't yet be built from intent alone. Callers are named by a header, not authenticated.
- **The offline coder is a compiler, not a model.** Without `ANTHROPIC_API_KEY`, the spec is written alongside each conversation and code comes from a deterministic generator. The Claude path (capture and coding, with a repair loop) is tested against scripted replies and rehearsed end to end against a stand-in API, not yet against a live model. `deploy/kind` makes the live run a contained, observable, one-command step ([docs/USER-GUIDE.md](docs/USER-GUIDE.md)); model-written code still runs as the same user as the factory there ([docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#trust-boundaries)).
- **Canaries judge harm narrowly.** A canary is stopped for server errors; it records latency but doesn't guard it, and it doesn't yet compare refusals or business outcomes between versions while both are live (Observe does that after promotion).
- **Shared state is a file.** Versions share data through a JSON file read on every request, which is what makes canaries safe here but wouldn't scale; real services would share a database, and MixedCheck would need to cover schema changes there.
- **Production findings are narrow.** Observe finds new refusals and server errors; it doesn't yet notice latency, business outcomes, or behaviour that's wrong but not an error. The answers it offers are fixed per finding type; a model could propose richer intent changes.
- **Behaviour diffs replay from empty state.** Observe now uses a snapshot taken at each release; pre-release diffs still replay all traffic from empty.
