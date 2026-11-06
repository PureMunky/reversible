# User guide

This guide is for running the system and working with what it builds. The system builds software from intent: you describe what you want, the factory generates the service, the review shows how behaviour would change, and releases are decisions you can rewind. You never read or edit the code to know whether it's right.

It covers running the demos in a local kind cluster (offline, rehearsed, then with a real Claude key), reading what they print, using and inspecting the service they build, and changing intent. How the pieces fit together is in [ARCHITECTURE.md](ARCHITECTURE.md); what leaves your machine with a key is in [RUNNING-WITH-CLAUDE.md](RUNNING-WITH-CLAUDE.md).

## Who does what

| Role | In the demos | What they touch |
|---|---|---|
| **Owner** | Phil (feature flags), Priya (incident tracker) | Intent: conversations, answers to the spec's questions, sign-off on behaviour, answers to production's questions. Never code. |
| **Caller** | Teams, bots, dashboards, named by the `X-Caller` header | The service's HTTP API |
| **Operator** | You | The environment: `deploy/kind/run.sh`, the key, the inspector |

The owner's choices in the demos are scripted (which answer to pick, when to sign off), so each run tells the same story and CI can assert it. Making those choices yourself through an interface is the next step for the system; for now you change intent by editing spec files (see [Changing the intent](#changing-the-intent)).

The domains are examples. The incident tracker and the feature-flag service come from the same factory with nothing domain-specific in it.

## What you need

- Docker, [kind](https://kind.sigs.k8s.io/) and kubectl. About 4 GB of free memory and a few GB of disk.
- For live runs, an Anthropic API key. The key stays in the model gateway, never in the pod where model-written code runs, and each run has a hard budget. Even so, a key made for this, with its own spending limit, is good practice ([trust boundaries](ARCHITECTURE.md#trust-boundaries)).
- Nothing else. The image carries the Go toolchain the factory builds generated code with.

## Quick start

```sh
deploy/kind/run.sh up        # cluster (with Calico), image, namespace, network rules
deploy/kind/run.sh check     # prove the network rules are enforced on this cluster
deploy/kind/run.sh offline   # build, change and release the incident tracker, no network
deploy/kind/run.sh open      # then browse http://localhost:8080/_factory/
```

`offline` streams the whole story (a few minutes), then the service keeps running in the cluster for up to two hours. `open` port-forwards it; leave it running in its own terminal.

## The ladder, from no risk to a live run

Each rung tests one more thing than the last. Don't skip `check`.

| Step | Command | What it proves | Cost |
|---|---|---|---|
| 1. Offline | `run.sh offline` (or `offline factory`) | The loop works end to end in the cluster, with the deterministic generator and no network at all | None |
| 2. Isolation | `run.sh check` | Offline pods reach nothing; live pods reach the Anthropic API and not the cluster | None |
| 3. Rehearsal | `run.sh rehearse` | The live path works end to end. The run token reaches the pod. Requests go through the model gateway and come back (one is refused as overloaded and retried). Replies are parsed, and the code in them is built by the factory and run in the sandbox. Afterwards, the sandbox is checked for visible credentials. A stand-in API (`cmd/fakeclaude`) writes the generator's code | None |
| 4. Key | `run.sh key` | Stores your key in a Secret that only the model gateway reads. Input is hidden; the key goes through stdin, never an argument or a file | None |
| 5. Live | `run.sh live` | Claude writes every version of the incident tracker's code from the intent; the examples, the review and the canary decide whether each ships | 3–9 model calls |
| 5b. Behaviours, live | `run.sh rehearse bookings`, then `run.sh live bookings` | Claude writes logic stated in words (pricing, no double-booking, opening hours, refunds) on top of generated structure. Rehearsal replays recorded replies through the gateway; live asks Claude | 2–6 model calls, cheap (~10k tokens in, ~1k out each) |
| 6. Live, from conversation | `run.sh live factory` | Claude drafts the feature-flag specs from conversation and writes the code | 5–12 model calls |

`live` explains what it's about to do and asks before starting (`YES=1` skips the prompt). Choose a model with `REVERSIBLE_MODEL=claude-opus-5-5 deploy/kind/run.sh live`; the default is `claude-sonnet-5-5`.

Every rehearse and live run gets a fresh token and a budget at the model gateway: 15 successful calls and 150,000 output tokens by default, enough for either demo with every repair attempt. Change it per run with `GATEWAY_MAX_CALLS=6 GATEWAY_MAX_OUTPUT_TOKENS=40000 deploy/kind/run.sh live`. A spent budget stops the run, with the build in progress failing on a `budget is spent` error; failed and refused calls don't count. When a rehearse or live run is serving, `run.sh` reports `Sandbox checked: generated code can't see the factory's processes or its run token`. If it ever prints a WARNING instead, stop and don't run live.

## Reading the story

Every run prints the same numbered sections. What to look for in each:

**1 · The owner describes what they need.** The conversation, then the spec's summary: `2 resources (services, incidents), 18 examples; the spec checker has 0 question(s)`. Questions marked `[blocking]` stop the build; `[clarify]` ones point at behaviour the examples don't pin down. In `live factory`, Claude's own questions print as `? (from Claude)`.

**2 · The factory generates, builds and verifies it.**

```
· model claude-sonnet-5-5: 2641 in / 3605 out tokens, 41s
spec b72e516553a4: 18/18 examples pass, 576 generated lines, 1 attempt(s)
```

The `·` lines are model calls, with tokens and time (rehearsal and live only). `attempt(s)` above 1 means the first code failed some examples and the model repaired it from the exact failures. If no attempt passes, the story stops here, the pod ends `Failed`, and the failures are listed. In a live run that's a result, not a malfunction: it's evidence of what the model couldn't do from this intent.

**3 · Sign-off; it goes live.** Every example with a ✓. The release is recorded in the ledger as a decision.

**4 · A week of production.** Scripted traffic, including requests the intent refuses. The census line (`4 services, 4 incidents (1 acknowledged, 1 resolved, 2 open)`) is real data read back from the service.

**5 · The owner changes the intent, not the code.** A new spec, a new build.

**6 · What would change, on real traffic and real data.** The core of the review:

```
replayed 29 recorded requests: 15 would behave differently
  ×8   new field commander
  ×2   new access rule: only on-call may change incidents
  ×2   rules for incident.status changed
  ×3   follows from earlier change (new access rule: only on-call may change incidents)
reading every existing record through both versions: ...
running both versions on the same data: each keeps the other's fields
```

Every group names the intent change that explains it. `follows from earlier change` means a later request differs only because an earlier one did. **`UNEXPLAINED`** means the new version behaves differently in a way nobody asked for, and sign-off is refused. A `⚠` under "running both versions" means one version would destroy data the other wrote. With a live model, this section is where you find out whether its code did only what the intent said.

**7 · Released as a canary.** The new version serves 20% of traffic beside the old one, on the same data:

```
+200 requests: running: 39/80 outcomes, ok +0.0%, no harm detected so far
+400 requests: promoted: ... no harm after 82 outcomes
```

`stopped:` means the canary did harm (server errors rose), the decision behind it was rewound, and the old version serves everything.

**8 · The factory watches what the new version did.** Findings: requests the new version refused or failed that the old one served, grouped by caller and traced to the intent change behind them.

**9 · Each finding becomes a question.** Answers are intent changes, and `→` marks the scripted owner's choice:

```
? ... status-page-bot has had 2 request(s) refused ... because of the new access rule ...
    1. keep refusing; status-page-bot must change
  → 2. let status-page-bot change incidents
    3. let anyone change incidents
```

**10 · The answers are the new intent.** v3 is generated with the production requests captured as examples, so the decision becomes part of the contract. Its behaviour changes are checked against the answers, it goes through a canary, and a week later there should be `no new findings`.

**What people did.** The human contribution, the generated line count (none of it written or edited by a person), and the ledger (`chain verifies ✓` means every record's signature and hash chain checked). In rehearsal and live runs, `model usage` totals the calls and tokens.

## Using the service

Once the story is over, the pod prints `Serving production on :8080` and `run.sh` says it's serving. With `run.sh open` running, the live version answers at `localhost:8080`. Requests are recorded like any production traffic.

The incident tracker (`offline`, `rehearse`, `live`):

```sh
curl -s localhost:8080/services                      # list
curl -s 'localhost:8080/incidents?status=open'       # filter (only filterable fields; others are 400)
curl -s localhost:8080/incidents/inc-106             # read one

# Writes name a caller. Only on-call and the status page bot may change incidents:
curl -s -X POST localhost:8080/incidents -H 'X-Caller: on-call' \
  -d '{"id":"inc-200","service":"checkout","severity":"sev2","summary":"slow pages"}'
curl -s -X PATCH localhost:8080/incidents/inc-200 -H 'X-Caller: on-call' -d '{"status":"acknowledged"}'
curl -s -X PATCH localhost:8080/incidents/inc-200 -H 'X-Caller: on-call' -d '{"status":"resolved"}'
#   → 409: status can't change from acknowledged to resolved (mitigate first)
curl -s -X PATCH localhost:8080/incidents/inc-200 -H 'X-Caller: dashboards' -d '{"summary":"x"}'
#   → 403: dashboards may not change incidents
curl -s -X DELETE localhost:8080/services/checkout -H 'X-Caller: platform-team'
#   → 409: still referenced by incidents
```

The feature-flag service (`offline factory`, `live factory`) answers at `/flags` the same way.

What the responses mean, for any service the factory builds:

| Status | Meaning |
|---|---|
| 200, 201, 204 | Done (201 returns the created record; 204 is a delete) |
| 400 | The request breaks a field rule (pattern, type, range, enum, unknown field), references a record that doesn't exist, or filters on a field that isn't filterable |
| 403 | The caller isn't one of the resource's writers |
| 404 | No such record |
| 409 | A rule refused it: a duplicate key, a delete forbidden in this state, a delete of something still referenced, or a state change the lifecycle doesn't allow |

Every refusal's `error` message comes from the intent, including the `why` the owner gave.

## Inspecting what's live

`http://localhost:8080/_factory/` is the front page: what the service is for, how it was built, which version is live (and any canary), model usage, and its endpoints, writers and filters, all read from the intent. From there:

| Page | Shows |
|---|---|
| `/_factory/spec` | The spec the live version was built from: the source of truth |
| `/_factory/examples` | The contract: every example, and what the live version answered |
| `/_factory/why` | The decision chain behind the live version, back to the owner's words |
| `/_factory/traffic` | The last 50 production requests: serving version, caller, request, status |
| `/_factory/code` | The generated code. It's a cache: change the intent, not this |

Two habits worth having. Read `/why` before `/code`: the point of the system is that the decisions explain the software. And after using the service yourself, check `/traffic`, where your requests sit beside the scripted ones, attributed to the version that served them.

For the files themselves: `kubectl --context kind-reversible -n reversible exec -it job/incidents-offline -c factory -- sh`, then `ls /shared/incidents-demo-*/` (`builds/<spec hash>/main.go` for each version of the code, `production/` for the data and its per-release snapshots).

## Watching it from outside

You don't have to wait for the story to finish, or read logs, to see what the system is doing. From the moment a run starts its pod serves telemetry on port 8080, so start `run.sh open` right after `offline`, `rehearse` or `live` begins (in a second terminal) and:

- **`http://localhost:8080/_factory/live`**: the live view. Headline numbers (live version, canary, requests, server errors, builds, attempts, unexplained changes, findings, model calls and tokens, ledger records) and a feed of events, newest first, as they happen.
- **`curl -N 'localhost:8080/_factory/events?stream=1'`**: the same events as a stream, one JSON object per `data:` line. Add `&kind=canary` (or `build`, `model`, `finding`, `ledger`...) to follow one kind.
- **`curl -s localhost:8080/metrics`**: Prometheus metrics.

For dashboards, add Prometheus and Grafana to the cluster once:

```sh
deploy/kind/run.sh observe       # Prometheus + Grafana in namespace observe
deploy/kind/run.sh dashboards    # http://localhost:3000 (Grafana), :9090 (Prometheus)
```

The Grafana home page is the dashboard *Software built from intent*. It follows whichever demo is running, across runs. Things worth watching in it:

| Panel | What to look for |
|---|---|
| Live version, Canary, Releases | A canary appearing, then either a promote (it becomes live) or a canary_ended without a promote (it was stopped) |
| Requests/s by version | During a canary, two versions serving at once (live and canary roles) on the same data |
| Server error ratio by version | The canary's guardrail: a candidate whose errors rise above the live version's is stopped |
| Refusals/s by status | 403s and 409s rising after a release are what becomes findings |
| Coding attempts | In a live run, how often Claude's first attempt passed the examples |
| Review: behaviour changes | `explained: no` above zero means a release was blocked |
| Findings from production | Which callers were surprised by a new version |
| Model calls, Model tokens | A live run's cost as it happens |
| Gateway budget remaining, Gateway requests | What's left of the run's budget; `unauthorized` above zero means something other than this run's factory tried to use the gateway |
| Where generated code ran | In kind, everything under `sandbox` |

Every metric and event is listed in [ARCHITECTURE.md](ARCHITECTURE.md#telemetry). Outside kind, any demo run with `-serve :8080` serves the same telemetry; set `REVERSIBLE_EVENTS=events.jsonl` to also keep every event in a file.

## Live runs: what to watch

A live run isn't expected to reproduce the offline numbers. Claude's code differs from the generator's, so line counts and attempts differ; in `live factory` its specs differ too, so spec hashes and example counts change, and the scripted owner's answers may not match the findings it gets (unmatched questions print `(left for later)`). CI's assertions apply to the offline run only.

What's worth noting from each live run:

- **Attempts per build**, and which examples failed first. That's where the model and the intent disagreed.
- **The review.** Anything `UNEXPLAINED` is the review catching model-written behaviour nobody asked for, and the most important thing a live run can show.
- **The canary.** A stop means a defect only real traffic found.
- **`model usage`** at the end, against your key's spend. The Model tokens panel (or `curl -s localhost:8080/metrics | grep model_tokens`) shows it while the run is in progress.
- In `live factory`: **Claude's questions** compared with the checker's, and how its spec differs from the hand-written one (`/_factory/spec`).

`run.sh logs` reprints the whole story later; it's worth saving each live run's output.

## Changing the intent

Today intent lives in spec files in the image: `cmd/incidents/intent/incidents-1.json` and `-2.json`, and `cmd/factory/intent/*.json` (the offline stand-ins for what Claude drafts live). To change what gets built, edit a spec, rebuild the image and run again:

```sh
$EDITOR cmd/incidents/intent/incidents-2.json
go run ./cmd/incidents                # quickest check, on your machine (offline, no network)
deploy/kind/run.sh image              # rebuild and load the image
deploy/kind/run.sh offline            # or rehearse, or live
```

If the spec isn't ready, the story stops at step 1 with the checker's questions. Logic the fields can't express goes in a resource's `behaviours`: a rule in words, of kind `computed` (sets a field marked `"computed": true`), `check` (may refuse a create or update) or `action` (`POST /<plural>/{key}/<action>`). Each needs at least one example naming it with `"behaviour": "<name>"`. A model writes them; `cmd/bookings/intent/bookings-1.json` is a complete example. Offline, a changed behaviour has no recorded reply, so the story stops and asks for a live run. `go run ./cmd/bookings -record cmd/bookings/recordings` with a key records new replies.

The format, briefly (the incident specs are complete examples of everything else):

```jsonc
{
  "service": "name", "purpose": "what it's for and who uses it", "owner": "who is accountable",
  "resources": [{
    "name": "incident", "plural": "incidents", "key": "id",
    "writers": ["on-call"],                     // who may change these (anyone may read); omit for anyone
    "fields": [
      {"name": "id", "type": "string", "required": true, "pattern": "^inc-[0-9]+$"},
      {"name": "service", "type": "string", "required": true, "references": "service", "filterable": true},
      {"name": "status", "type": "string", "enum": ["open", "resolved"], "default": "open",
       "transitions": {"open": ["resolved"]},   // allowed changes; a state with no entry can't change
       "why": "shown to whoever is refused"}
      // types: string, bool, int, number; also min/max for numbers
    ],
    "rules": [{"forbid_delete_when": {"field": "status", "equals": "open"}, "why": "...", "except_callers": ["cleanup-bot"]}]
  }],
  "examples": [                                 // run in order against a fresh instance; they are the contract
    {"name": "open one", "request": {"method": "POST", "path": "/incidents", "body": {...}, "caller": "on-call"},
     "expect": {"status": 201, "body": {"status": "open"}}}  // body is matched as a subset
  ]
}
```

The demos' traffic, conversations and scripted answers are written for their specs, so a large change may make later sections of the story less meaningful (findings may not match the scripted answers). Running a spec of your own, with your own traffic and decisions, needs the control plane described in [ARCHITECTURE.md](ARCHITECTURE.md#from-kind-to-a-real-environment-not-built), which isn't built yet.

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| `check` fails | Network rules not enforced (Calico not running) | `kubectl --context kind-reversible -n kube-system get pods -l k8s-app=calico-node`; if the cluster predates the Calico change, `run.sh down` then `up`. Don't run live until `check` passes |
| Pod stuck `Pending` | Not enough memory or CPU for the requests (500m, 512Mi) | Free resources, or check `kubectl describe pod` |
| `ErrImageNeverPull` | Image not loaded into kind | `run.sh image` |
| Live run: `authentication_error` | Wrong or revoked key (the gateway passes the API's answer through), or a stale run token (the gateway was reconfigured mid-run) | `run.sh key` again, then rerun |
| `permission_error: this run's budget is spent` | The run used its calls or output tokens | Rerun with a larger `GATEWAY_MAX_CALLS` / `GATEWAY_MAX_OUTPUT_TOKENS`, after checking why (attempts per build, in the story) |
| Live run: `upstream unreachable` | The gateway can't reach the API (network, or Calico not ready) | `run.sh check`; `kubectl --context kind-reversible -n reversible logs deploy/model-gateway` |
| Live run: repeated `(retrying)` then failure | Rate limits or overload | Wait and rerun; fewer concurrent uses of the key |
| Live run: `reply cut off at max_tokens` | The generated service needs a longer reply | Rerun with more room: `REVERSIBLE_MAX_TOKENS=32000 deploy/kind/run.sh live` (default 16000) |
| Story ends with `ended (Failed) before serving` | A step couldn't produce its evidence (no passing build, unexplained changes, an error) | The story above it says which; `run.sh logs` reprints it |
| `open`: connection refused | The pod hasn't started yet, or has ended | `run.sh status` |
| Grafana panels empty | No demo running, or Prometheus can't reach it | Start a run; check `http://localhost:9090/targets` (the `reversible` target should be up) |
| Anything else | | `run.sh status`, `run.sh logs`, `kubectl --context kind-reversible -n reversible describe pods` |

## Cleaning up

```sh
deploy/kind/run.sh forget-key   # delete the key, and restart the gateway without it
deploy/kind/run.sh down         # delete the cluster, and everything in it
```

Then revoke the key in the Anthropic Console if you made it for this.

## Reference

`deploy/kind/run.sh` commands:

| Command | Does |
|---|---|
| `up` | Create the cluster (with Calico), build and load the image, apply the namespace, network rules, model gateway and rehearsal stand-in |
| `image` | Rebuild and reload the image (after changing code or intent) |
| `check` | Prove the network rules are enforced: offline pods reach nothing, demo pods only the gateway, the gateway only the API |
| `offline [incidents\|bookings\|factory]` | Run a demo with the deterministic generator, no network |
| `rehearse [incidents\|bookings]` | Run through the live path (gateway, sandbox) against the stand-in API (bookings: recorded behaviour code) |
| `key` | Store the API key for the model gateway (prompted, hidden; or from `ANTHROPIC_API_KEY`) |
| `live [incidents\|bookings\|factory]` | Run a demo with Claude |
| `open` | Port-forward the running demo to `localhost:${LOCAL_PORT:-8080}` (works from the start of a run, for telemetry) |
| `observe` | Add Prometheus and Grafana (namespace `observe`), scraping the demo |
| `dashboards` | Port-forward Grafana to `localhost:3000` and Prometheus to `localhost:9090` |
| `logs`, `status` | The current run's story; what's running, whether a key is stored, and the gateway's last log lines (budget used) |
| `forget-key`, `down` | Remove the key; delete the cluster |

Environment variables:

| Variable | Read by | Default |
|---|---|---|
| `REVERSIBLE_MODEL`, `REVERSIBLE_MAX_TOKENS` | `run.sh live` (passed into the job) | `claude-sonnet-5-5`, `16000` |
| `LOCAL_PORT` | `run.sh open` | `8080` |
| `YES=1` | `run.sh live` | Asks first |
| `ANTHROPIC_API_KEY` | `run.sh key`, and the demos when run outside kind | Prompted |
| `GATEWAY_MAX_CALLS`, `GATEWAY_MAX_OUTPUT_TOKENS` | `run.sh rehearse` and `live`: the run's budget at the gateway | `15`, `150000` |
| `ANTHROPIC_BASE_URL` | The demos (in kind, set to the gateway's address) | `https://api.anthropic.com` |
| `REVERSIBLE_EVENTS` | The demos: also write every event as a JSON line to `stderr` or a file | Off |
| `STORY_TIMEOUT` | `run.sh`: how long to stream a story before giving up, in seconds | `2700` |

Outside kind, every demo runs with `go run` (see [README](../README.md)); add `-serve :8080` to any of them for the same serving and inspector.
