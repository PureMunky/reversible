# reversible

**Agents don't need permission. They need undo.**

`reversible` is a prototype effect system for agentic software, written in Go. Instead of gating every action behind a human, every action an agent takes is *declared up front* and *undoable by construction*. Only the actions that truly can't be undone get routed to a person, and the architecture's job is to keep that set as small as possible.

It is the first building block of a larger idea: software built for agents, where intent and consequences are the primary artifacts and code is something that gets regenerated in between.

## The model

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

## Run the demo

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
| `pending` | Reversible effects applied, escrowed effects held |
| `committed` | Everything applied |
| `rolled_back` | Undone, and nothing remains in the world |
| `partially_rolled_back` | Undone, but some effects couldn't be (see `Result.Stranded()`) |
| `rejected` | A gate denied an irreversible effect, so nothing ran |

## Limitations (it's a prototype)

- **Escrow lives in memory.** Effects are Go closures, so held escrow doesn't survive a restart. A durable version needs effects to be serializable descriptions that a registry rehydrates.
- **The ledger is best-effort.** A production runtime would write ahead and refuse to act if the ledger write fails.
- **Compensation is trusted.** An `Undo` that lies or partially succeeds isn't detected. Verifying that compensation actually restored the prior state is an open problem.
- **Single process.** No distributed coordination across services yet.

## Where this is going

This is step one of three foundations:

1. **Effect system** (this repo): agents act freely because everything is declared and undoable.
2. **Decision ledger:** a causal graph of intent → decision → artifact → outcome, so you can rewind a *decision*, not just an action.
3. **Counterfactual runtime:** run candidate changes against replayed production traffic with every effect intercepted, so testing becomes watching what *would* have happened.

## License

MIT
