# Running with Claude

Everything in this repo runs offline by default. This guide is for when you want a real model in the loop, and it is written to be read **before** you set a key: what the model does, what leaves your machine, what runs on it, and a ladder of steps from no risk to a live run.

The root idea being tested: a person describes software in conversation, and a model turns that into intent and then into working code, with the system (not a person reading code) deciding whether the result is right. Offline, a deterministic generator stands in for the model, so the demos prove the plumbing. Only a live run tests the claim itself.

## What the model does, and nothing else

A model is called in exactly three places. Everything else (checking specs, building, running examples, replaying traffic, diffing behaviour, observing production, the ledger) is deterministic code in this repo.

| Where | Called by | Sends | Gets back | What happens to it |
|---|---|---|---|---|
| **Conversation → spec** | `factory.Capture` (`cmd/factory`) | The conversation, the current spec (if any) or a format example, and the service contract | A spec as JSON, plus the model's own questions | Parsed strictly (unknown fields rejected), then checked by `Spec.Check`; blocking questions stop the build |
| **Spec → code** | `factory.ModelCoder` (`cmd/factory`) | The service contract and the full spec; on a retry, its previous `main.go` and exactly which checks failed | One Go file in a code block | **Compiled and run on your machine** as a separate process, then checked against the spec's examples (see below) |
| **Evidence → policy** | `synth.ModelGenerator` (`cmd/designer`) | The intent spec, effect types, example requests, the champion policy and every trial's outcomes | Policies as JSON | Parsed as data, never executed; replayed through the gate before any (simulated) customer sees it |

The prompts are in the code (`CaptureSystemPrompt`, `CoderSystemPrompt` in `factory`, `SystemPrompt` in `synth`). To see the exact text that would be sent at every step of the factory demo, without sending anything:

```sh
go run ./cmd/factory -show-prompts
```

## What leaves your machine

Only the prompts above, sent to `https://api.anthropic.com/v1/messages` by `model.Anthropic` (about 150 lines of standard-library Go in `model/model.go`; read it). There is no telemetry and no other network call. The demos' "customers", "teams" and "conversations" are made up; nothing from your environment is included unless you put it in a spec or conversation yourself.

## What runs on your machine

This is the part to understand before running the factory live.

- **Model-written Go is compiled and executed** (`factory/build.go`): written to a temporary directory, built with `go build` (standard library only, `CGO_ENABLED=0`, no module downloads), and run as its own process listening on `127.0.0.1` on a random port.
- **It runs with your user's permissions.** The gate checks *behaviour*: it runs the examples against the process and compares responses. It does not inspect what the code does besides answering requests. A generated program could read files or open network connections, and the examples would never notice.
- **So run the factory live in a container or VM**, not on your workstation directly. The kind environment does this for you: the key stays in a model gateway that enforces a per-run budget, generated code runs in a sandbox container that can't see the factory or any credential, the pod running it has no route out but the gateway, and the whole path can be rehearsed first for free: see [USER-GUIDE.md](USER-GUIDE.md) and, for what it does and doesn't contain, [ARCHITECTURE.md](ARCHITECTURE.md#trust-boundaries). Or, with plain Docker:

  ```sh
  docker run --rm -it -v "$PWD":/src -w /src \
    -e ANTHROPIC_API_KEY golang:1.24 \
    go run ./cmd/factory -keep
  ```

  The container needs outbound network to reach the API; it does not need your home directory.
- **Processes are cleaned up**, including when the demo fails partway through. If you ever interrupt it hard (`kill -9`), check for leftovers with `pgrep -fa factory-demo` and remove them.

The designer demo (`cmd/designer`) never executes model output: policies are data. It's the safe first live run.

## A ladder, from no risk to a live run

1. **Read** `PRINCIPLES.md` (the idea and where each piece fits), then the package comment at the top of `factory/spec.go` (the whole pipeline in one paragraph).
2. **Run everything offline.** No key, no network:

   ```sh
   go test -race ./...
   go run ./cmd/factory
   go run ./cmd/factory -faulty-coder   # a bad build that passes every example, caught before release
   ```
3. **Read what was generated.** `-keep` leaves the work directory in place and prints where it is:

   ```sh
   go run ./cmd/factory -keep
   ```

   Look at `builds/<spec hash>/main.go` (the generated service for each intent) and `production/flags.json*` (production data, with a snapshot per release).
4. **Read every prompt.** `go run ./cmd/factory -show-prompts` prints what a model would be sent for each spec draft and each build, and calls nothing.
5. **First live run: model writes data only.** Use a key with a spending limit, set only in your shell:

   ```sh
   ANTHROPIC_API_KEY=sk-... go run ./cmd/designer -rounds 3 -show-prompt
   ```

   Nothing the model writes is executed. Expect a handful of calls. Compare its policies and rationales with the offline stand-in's.
6. **Live factory, in a container** (see above). Claude drafts both specs from the conversations and writes all three services. Watch for:
   - **Its questions.** Printed as `? (from Claude)`. Does it ask what `Spec.Check` would, or more?
   - **Attempts.** Each build's report says how many tries the repair loop needed.
   - **Unexplained behaviour changes.** If Claude's v2 changes something nobody asked for, sign-off is refused, the same as with `-faulty-coder`.
   - **Differences from the offline run.** Claude's specs won't match the hand-written ones exactly, so spec hashes, example counts and line counts will differ. That's expected; the CI assertions apply to the offline run only.

## Choosing a model, and cost

- The default model is `claude-sonnet-5-5`. Override with `REVERSIBLE_MODEL`, for example `REVERSIBLE_MODEL=claude-opus-5-5`.
- A full factory run makes roughly 5 to 12 calls: about two spec drafts, then one to three coding attempts for each of three builds; the incident tracker makes 3 to 9 (coding only). Each coding call sends a few thousand tokens and gets back several thousand (the whole service, up to `REVERSIBLE_MAX_TOKENS`, default 16000). Every call's tokens are logged and counted in `reversible_model_tokens_total`. Check current pricing on Anthropic's site rather than trusting an estimate here.
- Keys are read only from the environment (`model.FromEnv`). Don't put one in a file in this repo.

## Known gaps in the live path

- **Tested against scripted replies, not a live model.** The client, prompts, parsing and repair loop are covered by tests with canned responses and a mock API server; how well a given model does is what a live run will show.
- **One resource per service.** The spec language is small. Asking Claude for something beyond it (several resources, auth) will produce questions or a spec the checker rejects.
- **No sandbox of its own.** Isolation is up to you (container or VM), as above.
