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

The foundations exist. The pieces govern *how changes reach the world safely*:

| Principle | Built | Package |
|---|---|---|
| Intent is the source of truth | Specs compiled to checks; ambiguity comes back as questions | `intent` |
| History is decisions, not diffs | Causal, signed, hash-chained decision ledger with rewind | `decision` |
| Reversibility replaces permission | Declared, escrowed, compensable effects; crash recovery | `reversible` |
| Behaviour is reviewed, not code | Replay of recorded traffic; behaviour diffs; invariants; simulators | `counterfactual` |
| Releases become evolution | Gated challengers, significance testing, automatic rewind | `evolve` |
| Production is an input | Outcomes calibrate the models and drive the next proposals | `counterfactual`, `synth` |
| Code is generated from intent | A model proposes *policies* (data) from the spec and evidence | `synth`, `policy`, `model` |

**The gap:** so far the thing being generated and evolved is a *policy* (a small rules language), and every demo exercises it on pricing. The root idea needs the artifact to be **the software itself**: a conversation produces intent, a model generates a real service, the system builds and tests it, shows the behaviour diff, gets product sign-off, releases it by evolution, and regenerates it when intent changes, with no human editing code.

That is the next milestone. Everything built so far is the plumbing underneath it.
