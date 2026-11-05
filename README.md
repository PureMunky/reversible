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
- **The ledger is an audit trail, not the source of truth.** Recovery depends on the store; the ledger is best-effort. Unifying them is part of the decision-ledger work.
- **One process at a time per store.** There's no locking or leasing, so two processes sharing a state directory can conflict.
- **Compensation is trusted.** An `Undo` that lies or partially succeeds isn't detected. Verifying that compensation actually restored the prior state is an open problem.
- **Single process.** No distributed coordination across services yet.

## Where this is going

This is step one of three foundations:

1. **Effect system** (this repo): agents act freely because everything is declared, undoable, and survives a crash.
2. **Decision ledger:** a causal graph of intent → decision → artifact → outcome, so you can rewind a *decision*, not just an action.
3. **Counterfactual runtime:** run candidate changes against replayed production traffic with every effect intercepted, so testing becomes watching what *would* have happened.

## License

MIT
