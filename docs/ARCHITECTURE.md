# Architecture

This repo is a working prototype of one idea: **software is built from intent**. People say what they want; that becomes a precise spec; code is generated from the spec and treated as a cache; changes are judged by how behaviour changes on real traffic and data, not by reading diffs; releases are undoable decisions in a signed ledger; and what production does flows back as questions whose answers are new intent.

This document describes how the pieces fit together when the system actually runs: first the logical architecture, then exactly what runs where in the kind environment (`deploy/kind`), then what a production deployment would need that isn't built yet. Domains in the demos (feature flags, incidents) are examples; nothing described here depends on them.

For how to operate it, see [USER-GUIDE.md](USER-GUIDE.md). For what leaves your machine when a model is involved, see [RUNNING-WITH-CLAUDE.md](RUNNING-WITH-CLAUDE.md).

## The loop

```mermaid
flowchart LR
    P([Owner]) -- conversation --> C[Capture<br/>model]
    C -- draft spec --> K{Check}
    K -- questions --> P
    K -- ready spec --> G[Coder<br/>model or generator]
    G -- main.go --> B[Build + examples]
    B -- failures --> G
    B -- candidate --> R[Review<br/>replay · existing data · mixed versions]
    R -- behaviour changes,<br/>each traced to intent --> P
    P -- sign-off --> L[(Decision ledger)]
    L --> Y[Canary<br/>router + evolution]
    Y -- promote / rewind --> L
    Y --> T[Production traffic]
    T --> O[Observe]
    O -- findings as questions,<br/>answers as intent --> P
```

A person appears at four points only: stating intent, answering questions, signing off on behaviour, and answering production's questions. Everything between is mechanical, and every mechanical step either passes evidence forward or stops.

## Components

| Component | Package | What it does | State it keeps |
|---|---|---|---|
| **Spec** | `factory` (`spec.go`) | The source of truth: resources, fields, references, lifecycles (`transitions`), filters, writers, rules, and examples that are the contract. `Check` returns blocking and clarifying questions; nothing blocked is built. A spec's hash names everything built from it. | None (it's data) |
| **Capture** | `factory` (`capture.go`) | Asks a model to turn a conversation (plus the current spec, if any) into a spec and its own questions. The reply is parsed strictly and then checked like any other spec. | None |
| **Coder** | `factory` (`codegen.go`, `coder.go`) | Writes the service from the spec. `Codegen` is a deterministic compiler for everything structural. `BehaviourCoder` adds a model for behaviours (logic stated in words): the generator writes `main.go` with fixed hooks, the model writes `behaviours.go`. `ModelCoder` has a model write the whole file. On failure, each model coder gets back its previous code and exactly which checks failed. | None |
| **Recordings** | `model` (`recording.go`) | `Recorder` saves each model reply under the hash of the exact prompt; `Replayer` answers only those prompts. Offline runs and CI replay recorded replies, so a changed spec can never be answered by a stale one. | Reply files |
| **Factory** | `factory` (`factory.go`, `build.go`) | Builds the code with `go build` (standard library only, no network), starts it as a separate process, runs every example in order against a fresh instance, retries the coder up to three times. Caches each build by spec hash. | `builds/<spec hash>/` |
| **Generated service** | output | A small HTTP service meeting a fixed contract: listens on `ADDR`, prints `LISTENING <url>`, keeps state in the JSON file at `DATA`, reads it on every request, preserves fields and resources it doesn't know (so two versions can share data), and treats the `X-Caller` header as the caller's identity. | The `DATA` file |
| **Router** | `factory` (`router.go`) | Production's front door. Forwards each request to the live version, or to a canary for a share of requests, and records every exchange (request, response, serving version, latency). Snapshots the data at each release. | Traffic log (memory), data snapshots |
| **Review** | `factory` (`diff.go`, `mixed.go`, `canary.go`) | Before release: replays all recorded traffic through both versions; reads every existing record through both; runs both on the same data to check neither destroys the other's fields. Every difference is explained by a specific intent change ("new access rule: only on-call may change incidents") or flagged as unexplained, which blocks sign-off. | None |
| **Canary** | `factory` (`canary.go`) + `evolve` | Runs the candidate beside the live version on a share of real traffic (20% in the demos, assigned by salted SHA-256 so it's stable and uncorrelated). Each outcome is attributed to the version that served it. A sequential test with alpha spending (6α/π²k²) promotes on evidence of no harm, or retires the candidate and rewinds the decision behind it when server errors rise. | Per-version outcomes |
| **Observe / Propose** | `factory` (`observe.go`) | After a release, replays every request since then through the previous version, starting from the release snapshot. New refusals and server errors become findings, grouped by caller and traced to the intent change behind them. Each finding becomes a question with concrete answers (keep refusing, exempt the caller, grant access, allow the transition, drop the rule), and each answer is a new spec that includes the production request as an example. | None |
| **Decision ledger** | `decision` | Goals, decisions, outcomes and the effects executed under them, as a graph. Every record is signed with the ed25519 key of the actor responsible and hash-chained to the previous one. `Explain` answers "why is this live?"; `Rewind` undoes a decision and everything downstream of it. | Records (pluggable `decision.Log`) |
| **Effect runtime** | `reversible` (root package) | Every change to the world (a release, a canary, a promotion) is an effect declared with its inverse. Intents run to completion or roll back, with crash recovery and idempotency. | Effect journal |
| **Model client** | `model` | Calls the Messages API with the standard library. 16,000-token replies by default (`REVERSIBLE_MAX_TOKENS`); a reply cut off at the limit is an error, never parsed as complete; 429, 5xx and network errors are retried with backoff; token usage is logged per call and totalled. | Usage counters |
| **Runner** | `cmd/runner` (`factory/runner.go`) | Starts and stops generated services on the factory's behalf, somewhere else: with `REVERSIBLE_RUNNER` set, `factory.Start` asks it instead of executing anything itself. Only binaries and data under its root may run. | The processes it started |
| **Model gateway** | `cmd/modelgateway` | Stands in for the Messages API: checks a per-run token, enforces a budget of calls and output tokens, and forwards to the real API with the real key. The only component that holds the key. | Budget used this run |
| **Inspector** | `cmd/internal/serve` | Serves production, `/_factory/` (what's live and why, the spec, the examples and what the live version answered, recent traffic, the generated code) and telemetry, from the start of a run. `/_factory/ready` turns 200 once the story is over. | None |
| **Telemetry** | `telemetry` | Prometheus metrics (`/metrics`) and a stream of structured events (`/_factory/events`, optionally to a file) from every step: specs checked, builds and attempts, model calls, reviews, routing changes, canary evaluations, findings and ledger records. A live view (`/_factory/live`) follows both. | Metrics, the last 2,000 events |

## The life of a change

```mermaid
sequenceDiagram
    actor Owner
    participant F as Factory
    participant M as Model
    participant V2 as Candidate (process)
    participant R as Router
    participant V1 as Live (process)
    participant L as Ledger

    Owner->>F: changed intent (spec v2)
    F->>F: Check: no blocking questions
    F->>M: contract + spec v2
    M-->>F: main.go
    F->>V2: go build, start on a fresh data file, run every example
    alt an example fails
        F->>M: previous code + exactly which checks failed (up to 3 attempts)
    end
    F->>F: review: replay traffic, read existing data, mixed-version check
    F-->>Owner: every behaviour change, each traced to the intent that explains it
    Owner->>L: sign-off (signed decision)
    L->>R: canary effect: V2 serves 20% beside V1, on the same data
    loop every evaluation
        R->>R: outcomes attributed per version
    end
    alt evidence of no harm
        L->>R: promote effect: V2 is live
    else harm (server errors rise)
        L->>L: rewind the decision: canary undone, V1 serves everything
    end
    R->>F: traffic since release
    F->>F: Observe: replay through V1 from the release snapshot
    F-->>Owner: findings as questions, each answer a new spec (v3)
```

Three properties hold throughout:

- **Nothing reaches production without evidence.** Examples, then the review, then a canary judged by statistics. A step that can't produce its evidence stops the change.
- **Every change is undoable by construction.** Releases, canaries and promotions are effects with inverses, executed under decisions. Rewinding a decision undoes its effects in reverse order; the data stays where it is, because every version reads data written by any other.
- **Code is never the thing reviewed or kept.** It's regenerated whenever intent changes and cached by spec hash; reaffirming an old intent is served from the cache with nothing regenerated.

## Running in kind

`deploy/kind/run.sh up` creates a one-node kind cluster named `reversible` with Calico as its network plugin (so NetworkPolicy is enforced), builds the image and loads it, and applies the namespace, the network rules, the model gateway and the rehearsal stand-in. A demo run is a Kubernetes Job; its pod runs the whole story, then keeps serving.

The layout follows one rule: **the code a model writes runs where there's nothing to take and nowhere to send it.** The key lives only in the model gateway; the factory holds a per-run token the gateway accepts; generated services run in a sandbox container that can't see the factory, and the pod's only route out is the gateway.

```mermaid
flowchart TB
    subgraph laptop[Your machine]
        U([you]) -- "run.sh open<br/>(port-forward)" --> PF[localhost:8080]
    end
    subgraph kind[kind cluster: reversible]
        subgraph ns[namespace reversible · pod security: restricted]
            subgraph job[Job pod: the demo · network: gateway only, no DNS]
                subgraph fc[container: factory]
                    D[demo process<br/>factory · router · ledger · canary<br/>:8080 production · inspector · telemetry<br/>env: run token]
                    GB[go build]
                    D --> GB
                end
                subgraph sb[container: sandbox · own process namespace · no secrets]
                    RN[runner :7070]
                    G1[generated v1]
                    G2[generated v2]
                    RN --> G1
                    RN --> G2
                end
                DATA[(shared emptyDir /shared<br/>builds · production data · snapshots)]
                D -- "start / stop (localhost)" --> RN
                GB --- DATA
                G1 --- DATA
                G2 --- DATA
            end
            GW[model gateway<br/>holds the key · checks the run token<br/>enforces the run's budget]
            K[[Secret anthropic<br/>read only by the gateway]]
            FAKE[fakeclaude<br/>rehearsal stand-in]
            SVC[Service demo :8080]
            K -.-> GW
        end
        subgraph obs[namespace observe · optional]
            PR[Prometheus]
            GF[Grafana<br/>dashboard]
            PR --> GF
        end
    end
    PF --> SVC --> D
    U -- "run.sh dashboards" --> GF
    PR -- "scrape /metrics" --> SVC
    PR -- "scrape /metrics" --> GW
    D -- "Messages API, run token<br/>(rehearse, live)" --> GW
    GW -- rehearse --> FAKE
    GW -- "live: HTTPS, real key" --> API[(api.anthropic.com)]
```

**Inside the demo pod** there are two containers:

- **factory** runs the demo binary (`incidents` or `factory`) as PID 1. It holds the factory, the router, the canary, the decision ledger and telemetry, all in memory, and serves production, the inspector and telemetry on `:8080` from the start. For each version of the intent it runs `go build` in `/shared/<demo>-demo-*/builds/<spec hash>/` (standard library only; `GOPROXY=off` makes module downloads impossible). Compiling Go doesn't execute it. In rehearse and live runs its environment holds the run's gateway token as `ANTHROPIC_API_KEY`, and the gateway's address as `ANTHROPIC_BASE_URL`.
- **sandbox** runs `cmd/runner`, as a native sidecar (it starts before the factory and stops after it). It's the only place generated code is executed: the factory asks it, over localhost, to start each built service with only `ADDR`, `DATA` and `TMPDIR` in its environment. Examples, replays, mixed-version checks, canaries and production all run there. The sandbox has its own process namespace (`shareProcessNamespace: false`), so generated code can't list the factory's processes, read its environment, or touch its memory. It has no secrets, no token, and no service-account credentials.
- They share a volume (`/shared`: builds, production data and snapshots) and the pod's network (localhost), and nothing else.

**The model gateway** (`cmd/modelgateway`) is its own Deployment, and the only pod that holds the key (from Secret `anthropic`) or can reach the internet. Each rehearse or live run gets a fresh random token. The demo pod presents the token as its API key; the gateway checks it in constant time, swaps in the real key, and forwards to `api.anthropic.com` (live) or `fakeclaude` (rehearse, which never receives a real key). It enforces the run's budget, 15 successful calls and 150,000 output tokens by default (`GATEWAY_MAX_CALLS`, `GATEWAY_MAX_OUTPUT_TOKENS`). Refused or failed calls aren't charged. A spent budget answers 403, which the client doesn't retry. It forwards only `POST /v1/messages`, and logs every call with what's left of the budget.

**The three modes** differ only in what's injected and what the network allows:

| Mode | Coder | Demo pod gets | Demo pod's network | Gateway |
|---|---|---|---|---|
| `offline` | Deterministic generator | Nothing (ConfigMap `offline`) | None at all | Not used |
| `rehearse` | `ModelCoder`, answered with the generator's code | The run token and the gateway's address | The gateway on 8080, nothing else: no DNS, no internet, no cluster API | Forwards to `fakeclaude` with a fake key |
| `live` | Claude writes the code (`incidents`), or drafts the specs and writes the code (`factory`) | The run token and the gateway's address, plus `REVERSIBLE_MODEL` | The same | Forwards to `api.anthropic.com` with the key; egress is DNS and public HTTPS only (private, link-local and CGNAT ranges excluded) |

The demo pod is given the gateway's IP address, so it needs no DNS. That closes DNS as a channel out.

`run.sh check` proves the rules hold on this cluster. It starts a pod wearing each set of labels and tries to connect (`cmd/reach`, a TCP probe):

- **offline:** reaches nothing.
- **demo:** reaches the gateway, and not the internet, DNS or the cluster API.
- **gateway:** reaches the Anthropic API, and not the cluster API.

Don't run live if it fails. kind's default CNI doesn't enforce NetworkPolicy (the first CI run of `check` showed an offline pod reaching the internet), so the cluster is created without it and `up` installs Calico, which does. `check` exists so this is never taken on trust. At the end of every rehearse and live run, `run.sh` also does what malicious generated code would: it looks for credentials in every process visible from the sandbox, and reports that it found none (with the factory's own environment as a control).

**Security context, every pod:** non-root (uid 65532), read-only root filesystem, all capabilities dropped, no privilege escalation, RuntimeDefault seccomp, no service-account token, no service-link environment variables, CPU/memory/disk limits. The demo Job has a two-hour deadline. The demo pod's writable space is two size-limited `emptyDir` volumes: `/work`, the factory's own (HOME and the Go build cache), and `/shared`.

**Getting in:** `kubectl port-forward` to the `demo` Service. Port-forwarding doesn't pass through NetworkPolicy. Inside the cluster, only Prometheus can reach the demo pod or the gateway, on their HTTP ports, when `run.sh observe` has been run.

**The CI job `kind`** runs this whole path on every push:

- `up`, then the three-way isolation check.
- An offline run, used through a port-forward and scraped by Prometheus.
- A rehearsal through the gateway, which includes a retried overload, the sandbox credential check, the gateway's budget accounting, and proof that every generated service ran in the sandbox and none in the factory container.

No key is used in CI.

## State: where it lives, what survives

| State | Where | Survives pod deletion? |
|---|---|---|
| Intent (specs) | Embedded in the image (`cmd/<demo>/intent/*.json`); live, the factory demo's specs are drafted in memory | Offline specs yes (in the image); drafted specs no |
| Generated code and binaries | `/shared/<demo>-demo-*/builds/<hash>/` (emptyDir) | No (it's a cache; same intent rebuilds it) |
| Production data and release snapshots | `/shared/<demo>-demo-*/production/` (emptyDir) | No |
| Traffic log | Router memory | No |
| Decision ledger and signing keys | Memory (`decision.NewMemoryLog`, keys generated at start) | No |
| Model usage | Client memory, printed at exit and shown at `/_factory/` | No |
| Metrics and events | Process memory (the last 2,000 events), plus Prometheus if `observe` is running | Metrics yes, in Prometheus (for a day); events only if written to a file outside the pod |
| API key | Secret `anthropic`, read only by the model gateway (and in its memory while it runs) | Yes, until `run.sh forget-key`, which also restarts the gateway without it |
| Run token and budget | Secret `gateway-token` (the demo pod) and `model-gateway-config` (the gateway); replaced every run | Until the next run |

Everything except the key is deliberately ephemeral in kind: each run is a complete, self-contained story. A real environment needs the opposite (see below).

## Trust boundaries

The model is treated as an untrusted author. What it writes is trusted only as far as the evidence goes, and wherever it runs, there's nothing for it to take and nowhere to send it.

**What's enforced:**

- **Specs from a model** are parsed strictly (unknown fields rejected) and checked like any spec. Blocking questions stop the build.
- **Code from a model** is compiled with no network and no toolchain switching (compiling Go doesn't execute it). It's released only after:
  - every example passes;
  - the review explains every behaviour change;
  - the owner signs off;
  - the canary shows no harm.
- **The key never enters the pod where model-written code runs.** It's read only by the model gateway. The demo pod holds a random per-run token instead, valid only at the gateway and only for this run's budget.
- **Generated code can't reach the token either.** It runs only in the sandbox container, which has its own process namespace and no secrets. Its environment is `ADDR`, `DATA` and `TMPDIR`; the factory's environment, processes and memory aren't visible to it. `run.sh` checks this at the end of every rehearse and live run, and CI checks it on every push.
- **No route out.** In rehearse and live runs the demo pod reaches only the gateway's port: no internet, no DNS, no cluster API, no other pods. Offline, it reaches nothing.
- **Spending has a hard ceiling outside the factory.** Whatever a run does (a repair loop that never converges, or code that somehow calls the gateway), the gateway stops it at the run's budget.
- **No cluster credentials anywhere**, no writable root, no capabilities. `check` proves the network rules on the running cluster.

**What's still open (smaller, and named):**

- **Generated code shares the pod's network and work volume with the factory.** From the sandbox it can call localhost ports: production and the inspector on 8080 (which it already serves), the runner, and the gateway (useless without the token). It can also read and write anything on `/shared`, including other versions' binaries and production data. That's a threat to the integrity of the demo's own artifacts, not a way to the key or out of the pod. Separate pods per version, each with its own volume, would close it (see below).
- **Same user id in both containers.** The process-namespace boundary is what hides the factory. Different uids would add a second boundary, but would need the shared volume's permissions reworked.
- **The gateway forwards whatever the factory sends,** within the budget. It doesn't inspect prompts or restrict models.
- **Callers aren't authenticated.** `X-Caller` is an asserted name; writers rules control behaviour, not security.
- **The ledger's keys live in the factory process,** so signatures show integrity of the record, not separation of duties.

## Telemetry

Everything above can be watched from outside while it happens, without reading code or logs. The metrics and events describe the path from intent to production: every one names a spec, a version, a release or a decision, never a line of code.

**Where it's served:** on the demo's port (8080 in kind) from the moment a run starts, beside production and the inspector.

| Endpoint | What |
|---|---|
| `/metrics` | Prometheus text format |
| `/_factory/events` | Every kept event as JSON. `?after=<seq>` for newer ones only, `?kind=<prefix>` to filter (`build`, `canary.stopped`), `?stream=1` (or `Accept: text/event-stream`) to replay and then follow as server-sent events |
| `/_factory/live` | One page following both: headline numbers and a feed of events as they happen |
| `REVERSIBLE_EVENTS=stderr` or `=<path>` | Also write every event as one JSON line, for log shippers |

**Metrics** (all prefixed `reversible_`; `service` and `version` labels name the intent, with the version being the first 12 characters of its spec hash):

| Metric | Type | Labels | Tells you |
|---|---|---|---|
| `live_version`, `canary_version` | gauge | service, version | Which version is live, and which is a canary (1), right now |
| `releases_total` | counter | service, action | release, canary_started, promote, canary_ended, rollback (a rewind), stop |
| `requests_total` | counter | service, version, role, method, resource, code | Production traffic, attributed to the version (and live or canary role) that served it |
| `request_seconds` | histogram | service, version, role | Latency per version |
| `builds_total` | counter | service, outcome | passed, failed, cached, blocked |
| `build_attempts_total` | counter | service, coder, result | passed, examples_failed, compile_failed, coder_error: how often the coder (model or generator) gets it right first |
| `examples_total` | counter | service, result | Contract checks run against builds |
| `build_seconds` | histogram | service | Time to a passing build, attempts included |
| `generated_lines` | gauge | service, version | Size of each version's generated code (a cache) |
| `reviews_total` | counter | service, outcome | clean, or blocked |
| `review_changes_total` | counter | service, check, explained | Behaviour differences by check (replay, existing_data) and whether intent explains them; `explained="no"` is what blocks |
| `canary_evaluations_total` | counter | service, result | running, promoted, stopped |
| `findings_total` | counter | service, kind, caller | New refusals and server errors after a release |
| `ledger_records_total` | counter | type, actor | goal, decision, outcome, execution, rewind |
| `model_calls_total` | counter | model, outcome | ok, retried, error, truncated |
| `model_tokens_total` | counter | model, direction | input, output |
| `model_call_seconds` | histogram | model | Model latency |
| `service_starts_total` | counter | where | Generated service processes started: `sandbox` (by the runner) or `local` (as the factory's child, outside kind). In kind, only `sandbox` |
| `gateway_requests_total` | counter | outcome | At the model gateway: forwarded, unauthorized (a wrong token), over_budget, upstream_error |
| `gateway_budget_remaining` | gauge | unit | What's left of this run's calls and output tokens |
| `gateway_tokens_total`, `gateway_upstream_responses_total` | counter | direction; code | Tokens through the gateway; upstream status codes |
| `uptime_seconds` | gauge | | Process uptime |

Requests to the scratch instances the factory starts for examples, replays and checks aren't counted as production traffic.

**Events** (each has `seq`, `time`, `kind` and `fields`):

| Kind | When | Key fields |
|---|---|---|
| `story.step`, `story.serving` | A demo starts a section; the story is over | step |
| `spec.checked` | Before any build | spec, blocking, clarify |
| `build.started`, `build.attempt`, `build.finished` | Each build and each coding attempt | spec, coder, attempt, result, failed (example names), error, outcome, lines, seconds |
| `model.call` | Each model API call | model, outcome, input_tokens, output_tokens, seconds, error |
| `review.finished` | A candidate is reviewed | live, candidate, outcome, replay_changes, replay_unexplained, existing_data_changes, mixed_version_problems, explained_by (each explanation with its count) |
| `routing.release`, `.canary_started`, `.promote`, `.canary_ended`, `.rollback`, `.stop` | What's live changes | from, to |
| `canary.evaluated`, `canary.promoted`, `canary.stopped` | Each canary evaluation | candidate, decision, result, samples, reason, summary |
| `finding`, `observe.finished` | Production compared with the previous version | kind, caller, requests, because, summary |
| `ledger.goal`, `.decision`, `.outcome`, `.execution`, `.rewind` | Each ledger record is written | id, actor, summary, parents, decision, intent, rewind_of, reason |
| `gateway.forwarded`, `.unauthorized`, `.over_budget`, `.upstream_error` | Each request at the model gateway (the gateway's own metrics and log) | model, tokens, calls_used, output_tokens_used |

**In kind**, `run.sh observe` adds Prometheus (scraping the demo's and the model gateway's `/metrics` every 5s; NetworkPolicy lets only Prometheus into the demo namespace, and only on those ports) and Grafana with the dashboard *Software built from intent* (`deploy/kind/observe/dashboard.json`). Its sections are what's live (versions, releases, traffic and errors per version, latency, refusals), building from intent (builds, attempts, examples, review changes explained or not, canary evaluations, findings, the ledger), the model (calls, tokens, latency), and containment (the gateway's remaining budget, its requests by outcome, and where generated code ran). The demo pod also carries `prometheus.io/*` annotations for any Prometheus that discovers pods.

**Not there yet:** no distributed traces (the standard library has no OpenTelemetry; events carry spec hashes and decision ids for correlation instead), and metrics reset with each run's process (Prometheus keeps their history).

## Failure modes

| What goes wrong | What happens | Where you see it |
|---|---|---|
| Intent is vague or contradictory | `Check` returns blocking questions; nothing is built | Story step 1; the drafter's own questions print as `? (from Claude)` |
| Model writes code that doesn't build or fails examples | Repair loop: previous code plus exactly which checks failed, up to 3 attempts; then the story stops and the pod ends `Failed` | The build report (`attempts`), each failure listed |
| Model reply hits the token limit | Error (`raise REVERSIBLE_MAX_TOKENS`), never parsed as complete | Logs |
| The run's budget is spent | The gateway answers 403 (`permission_error`), not retried; the build in progress fails | `reversible_gateway_budget_remaining`; gateway logs |
| API overloaded or rate-limited | Retried with backoff (2s, 4s, 8s) | `· model ... (retrying)` lines |
| Bad or missing key | Fails immediately (`authentication_error`), no retries. A wrong run token gets the same from the gateway | Logs |
| A build passes every example but changes behaviour nobody asked for | The review marks it `UNEXPLAINED`; no sign-off, nothing released | Story step 6 |
| A version would destroy fields another version wrote | The mixed-version check reports it; no sign-off | Story step 6 |
| A defect only real traffic triggers | The canary's error guardrail trips; the decision is rewound and the live version serves everything | Canary lines (`stopped: ...`); `cmd/factory -brittle-coder` shows it |
| A rule surprises a caller after release | A finding, then a question with intent changes as answers | Story steps 8–9 |
| The pod is deleted or hits its deadline | Everything but the key is gone (by design in kind) | `run.sh status` |

## Model calls

| Call | Made by | Per run (incidents / factory) | Size |
|---|---|---|---|
| Conversation → spec | `Capture` | — / 2 | A few thousand tokens in, up to a few thousand out |
| Spec → code | `ModelCoder` | 3–9 / 3–9 (3 builds, up to 3 attempts each) | ~3,000 tokens in plus the previous code on a retry; ~4,000–6,000 out for these services |
| Behaviours → code | `BehaviourCoder` (`cmd/bookings`) | 2–6 (2 builds, up to 3 attempts each) | ~9,500 tokens in (hooks, examples, spec, generated code); ~1,000 out |

The default model is `claude-sonnet-5-5` (`REVERSIBLE_MODEL` overrides it). Replies aren't streamed, so the HTTP timeout is ten minutes. Every call's tokens and duration are logged, and the total prints at exit. In kind, every call also passes through the model gateway, which caps the run (15 calls and 150,000 output tokens by default, enough for either demo with every repair attempt) and logs each call with what's left.

## From kind to a real environment (not built)

The kind environment runs the whole loop honestly, but as a self-contained story inside one pod. A real deployment keeps the same architecture and separates its parts. What each part would become:

| Today (one pod) | In a real environment | Why |
|---|---|---|
| Intent embedded in the image, or drafted in memory | Intent in Git: one repo of specs, changed by conversation, every change a decision; the factory watches it | Intent is the source of truth, so it needs history, review and access control of its own. This maps cleanly onto GitOps: intent repo → factory → image (the cache) → cluster |
| Demo binary orchestrating everything | A factory control plane with an API: submit a conversation or spec, get questions, see a review, sign off, answer findings | The owner's role becomes a product surface instead of a scripted story |
| Generated services in a sandbox container in the factory's pod | Each version its own Deployment, with its own volume and service account, a sandboxed runtime class (gVisor or Kata), no secrets, and egress denied | Closes what's left: no shared volume or localhost with the factory or with other versions |
| In-process router choosing canary requests | Traffic split at the ingress or mesh (weighted routes), with per-version outcome metrics exported to the evolution runtime | Real traffic, real infrastructure, same decision logic |
| JSON file read on every request | A database, with schema changes reviewed the same way (mixed-version checks become migration checks) | Scale and durability; two versions must still be able to share data |
| Ledger in memory, keys generated at start | A durable `decision.Log` (the interface exists), signing keys in a KMS or HSM per actor | History that outlives a run, and signatures that separate who decided from what ran |
| Key in a Secret read by a gateway with "public 443" egress | Key from a secret manager into the gateway; gateway egress through a proxy that allows only the model API; budgets per owner or team, not just per run | Hostname-level control, and spending policy that outlives a run |
| `X-Caller` header | Authenticated identity (OIDC, mTLS) mapped onto the spec's writers | Access rules become security, not just behaviour |
| Telemetry from one process | The same metrics scraped from every factory and service, events shipped to a log store, traces through OpenTelemetry where the standard-library rule allows, and alerts on unexplained changes, stopped canaries and findings | Watching many services and owners at once |
| Traffic log in memory | Sampled, retained request logs as the replay corpus, with personal data handled deliberately | Review and observation depend on representative traffic |

## What the architecture doesn't cover yet

These are limits of what can be built from intent at all, not of the deployment:

- **The spec language covers records, not computation.** Resources, references, lifecycles, filters and access are intent. Computed values, scheduled jobs (escalating an unacknowledged incident), reacting to events and calling other systems still have to be written by hand.
- **A live model hasn't been run through this loop yet.** Every run so far used the deterministic generator or the rehearsal stand-in. The first live runs are the first evidence of whether a model can write this software and whether the review catches what it gets wrong.
- **Canaries judge harm narrowly** (server errors), and **findings are narrow** (new refusals and server errors). Latency, business outcomes, and behaviour that's wrong without being an error aren't judged yet.

[PRINCIPLES.md](../PRINCIPLES.md) tracks these against the root idea.
