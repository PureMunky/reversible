#!/usr/bin/env bash
# Software built from intent, running in a local kind cluster.
#
#   deploy/kind/run.sh up                # cluster, image, namespace, network rules, model gateway
#   deploy/kind/run.sh check             # prove the network rules are enforced here
#   deploy/kind/run.sh offline [demo]    # deterministic coder, no network: the baseline
#   deploy/kind/run.sh rehearse [demo]   # the live path, through the gateway, to a stand-in API: no key, no cost
#   deploy/kind/run.sh key               # store your Anthropic key for the model gateway (input hidden)
#   deploy/kind/run.sh live [demo]       # Claude writes the code (incidents) or the spec and code (factory)
#   deploy/kind/run.sh open              # port-forward the running demo to localhost:8080
#   deploy/kind/run.sh observe           # add Prometheus and Grafana, scraping the demo's telemetry
#   deploy/kind/run.sh dashboards        # port-forward Grafana (localhost:3000) and Prometheus (:9090)
#   deploy/kind/run.sh logs | status | forget-key | down
#
# demo is incidents (default), bookings or factory. Set REVERSIBLE_MODEL to choose a
# model for live runs (REVERSIBLE_MAX_TOKENS for longer replies),
# GATEWAY_MAX_CALLS / GATEWAY_MAX_OUTPUT_TOKENS for a run's budget,
# LOCAL_PORT for open, YES=1 to skip the live prompt.
# docs/USER-GUIDE.md walks through it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cluster=reversible
ns=reversible
ctx="kind-$cluster"
calico=https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
k() { kubectl --context "$ctx" -n "$ns" "$@"; }

need() {
  for c in "$@"; do
    command -v "$c" >/dev/null || { echo "needs $c on PATH" >&2; exit 1; }
  done
}

image() {
  need docker kind
  echo "── building reversible:dev (the demos plus the Go toolchain they build generated code with)"
  docker build -t reversible:dev "$root"
  kind load docker-image reversible:dev --name "$cluster"
}

up() {
  need docker kind kubectl
  if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    kind create cluster --config "$here/kind-config.yaml"
  fi
  # Calico in place of kindnet, so NetworkPolicy is enforced (kind-config.yaml says why).
  echo "── installing Calico (enforces the network rules)"
  kubectl --context "$ctx" apply -f "$calico" >/dev/null
  kubectl --context "$ctx" -n kube-system rollout status daemonset/calico-node --timeout=300s
  kubectl --context "$ctx" wait --for=condition=Ready nodes --all --timeout=300s
  image
  kubectl --context "$ctx" apply -f "$here/manifests/base.yaml"
  kubectl --context "$ctx" apply -f "$here/manifests/fakeclaude.yaml"
  k rollout restart deployment/fakeclaude >/dev/null
  k rollout status deployment/fakeclaude --timeout=120s
  gateway rehearsal
  echo
  echo "Ready. Next: $0 check, then $0 offline"
}

check() {
  local ok=0 gw k8s dns
  gw=$(k get svc model-gateway -o jsonpath='{.spec.clusterIP}')
  k8s=$(kubectl --context "$ctx" -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}')
  dns=$(kubectl --context "$ctx" -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')
  for net in none model gateway; do
    k delete pod "isolation-$net" --ignore-not-found --wait >/dev/null
    sed -e "s/__NETWORK__/$net/g" -e "s/__GATEWAY_IP__/$gw/g" -e "s/__K8S_IP__/$k8s/g" -e "s/__DNS_IP__/$dns/g" \
      "$here/manifests/isolation-check.yaml" | kubectl --context "$ctx" apply -f - >/dev/null
  done
  for net in none model gateway; do
    k wait --for=jsonpath='{.status.phase}'=Succeeded "pod/isolation-$net" --timeout=90s >/dev/null 2>&1 || true
    k logs "isolation-$net" | sed 's/^/   /'
    [ "$(k get pod "isolation-$net" -o jsonpath='{.status.phase}')" = Succeeded ] || ok=1
    k delete pod "isolation-$net" --wait=false >/dev/null
  done
  if [ $ok -ne 0 ]; then
    echo "The network rules aren't behaving as intended on this cluster. Don't run live until they are" >&2
    echo "(This cluster needs Calico, which run.sh up installs: kubectl --context $ctx -n kube-system get pods -l k8s-app=calico-node)" >&2
    exit 1
  fi
  echo "Network rules enforced."
}

# gateway MODE: configure the model gateway for a run. A fresh random
# token each time: the demo pod gets the token, never the key. The token
# and settings go through stdin, never arguments or files.
gateway() {
  local mode=$1 upstream token
  case "$mode" in
    live) upstream=https://api.anthropic.com ;;
    rehearsal) upstream=http://fakeclaude.reversible.svc.cluster.local:8080 ;;
  esac
  token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  printf 'GATEWAY_MODE=%s\nGATEWAY_UPSTREAM=%s\nGATEWAY_TOKEN=%s\nGATEWAY_MAX_CALLS=%s\nGATEWAY_MAX_OUTPUT_TOKENS=%s\n' \
    "$mode" "$upstream" "$token" "${GATEWAY_MAX_CALLS:-15}" "${GATEWAY_MAX_OUTPUT_TOKENS:-150000}" |
    k create secret generic model-gateway-config --from-env-file=/dev/stdin --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
  printf 'ANTHROPIC_API_KEY=%s\n' "$token" |
    k create secret generic gateway-token --from-env-file=/dev/stdin --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
  kubectl --context "$ctx" apply -f "$here/manifests/gateway.yaml" >/dev/null
  k rollout restart deployment/model-gateway >/dev/null
  k rollout status deployment/model-gateway --timeout=120s >/dev/null
  echo "── model gateway: $mode, budget ${GATEWAY_MAX_CALLS:-15} calls / ${GATEWAY_MAX_OUTPUT_TOKENS:-150000} output tokens, new run token"
}

# The demo pod has no DNS, so it's given the gateway's address.
gateway_url() { echo "http://$(k get svc model-gateway -o jsonpath='{.spec.clusterIP}'):8080"; }

# run_demo NAME MODE NETWORK DEMO ENVKIND ENVNAME [GATEWAY_URL]
run_demo() {
  local name=$1 mode=$2 net=$3 demo=$4 envkind=$5 envname=$6 gwurl=${7:-}
  case "$demo" in incidents|factory|bookings) ;; *) echo "demo is incidents, bookings or factory" >&2; exit 1 ;; esac
  k delete job -l app.kubernetes.io/name=reversible-demo --ignore-not-found --wait >/dev/null
  sed -e "s/__NAME__/$name/g" -e "s/__MODE__/$mode/g" -e "s/__NETWORK__/$net/g" -e "s/__DEMO__/$demo/g" \
      -e "s/__ENVKIND__/$envkind/g" -e "s/__ENVNAME__/$envname/g" -e "s#__GATEWAY_URL__#$gwurl#g" -e "s/__MODEL__/${REVERSIBLE_MODEL:-}/g" -e "s/__MAXTOKENS__/${REVERSIBLE_MAX_TOKENS:-}/g" \
      "$here/manifests/demo-job.yaml" | kubectl --context "$ctx" apply -f - >/dev/null
  echo "── job/$name: $demo, $mode"
  # Wait for the container to start, stream its story, and stop streaming
  # once it's serving (ready) or has failed.
  local phase="" ready=""
  for _ in $(seq 1 120); do
    phase=$(k get pod -l job-name="$name" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)
    [ -n "$phase" ] && [ "$phase" != Pending ] && break
    sleep 1
  done
  # kubectl itself in the background (not through k, a function: $! would
  # be a subshell, and killing it would leave kubectl holding the output open).
  kubectl --context "$ctx" -n "$ns" logs -f -c factory "job/$name" &
  local logs=$!
  local deadline=$((SECONDS + ${STORY_TIMEOUT:-2700}))
  while [ $SECONDS -lt $deadline ]; do
    phase=$(k get pod -l job-name="$name" -o jsonpath='{.items[0].status.phase}')
    ready=$(k get pod -l job-name="$name" -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}')
    [ "$ready" = True ] && break
    [ "$phase" = Failed ] || [ "$phase" = Succeeded ] && break
    sleep 2
  done
  sleep 1
  kill $logs 2>/dev/null || true
  wait $logs 2>/dev/null || true
  echo
  if [ "$ready" = True ] && [ "$mode" != offline ]; then
    # What generated code would try: look for credentials in every process
    # it can see. From the sandbox, the factory's environment (with the run
    # token) must not be visible; from the factory it is, as a control.
    if k exec "job/$name" -c sandbox -- sh -c 'grep -qs ANTHROPIC_API_KEY /proc/[0-9]*/environ'; then
      echo "── WARNING: the sandbox can see a credential. Generated code isn't isolated from the factory here." >&2
    elif k exec "job/$name" -c factory -- sh -c 'grep -qs ANTHROPIC_API_KEY /proc/1/environ'; then
      echo "── Sandbox checked: generated code can't see the factory's processes or its run token (the API key is only in the gateway)."
    fi
  fi
  if [ "$ready" = True ]; then
    echo "── It's serving. $0 open, then:"
    echo "   http://localhost:${LOCAL_PORT:-8080}/_factory/   what's live and why; spec, examples, decisions, code"
    echo "   curl -s localhost:${LOCAL_PORT:-8080}/incidents?status=open     (use it; requests are recorded as production traffic)"
  else
    echo "── job/$name ended ($phase) or timed out before serving; the story above says why (k get pods for more)."
    return 1
  fi
}

offline() { run_demo "${1:-incidents}-offline" offline none "${1:-incidents}" configMapRef offline; }

rehearse() {
  local demo=${1:-incidents}
  if [ "$demo" = factory ]; then
    echo "The rehearsal stand-in writes code (and replays recorded behaviour code); the factory demo also needs a model to draft specs from conversation. Rehearse incidents or bookings." >&2
    exit 1
  fi
  gateway rehearsal
  run_demo "$demo-rehearsal" rehearsal model "$demo" secretRef gateway-token "$(gateway_url)"
}

key() {
  local key=${ANTHROPIC_API_KEY:-}
  if [ -z "$key" ]; then
    printf 'Anthropic API key (input hidden): '
    if [ -t 0 ]; then stty -echo; read -r key; stty echo; echo; else read -r key; fi
  fi
  [ -n "$key" ] || { echo "no key given" >&2; exit 1; }
  # Fed through stdin, so the key is never a command-line argument or a file.
  printf 'ANTHROPIC_API_KEY=%s\n' "$key" |
    kubectl --context "$ctx" -n "$ns" create secret generic anthropic --from-env-file=/dev/stdin --dry-run=client -o yaml |
    kubectl --context "$ctx" apply -f - >/dev/null
  echo "Stored in secret/anthropic in namespace $ns, read only by the model gateway. $0 forget-key removes it."
}

live() {
  local demo=${1:-incidents}
  k get secret anthropic >/dev/null 2>&1 || { echo "No key stored yet: $0 key" >&2; exit 1; }
  if [ "${YES:-}" != 1 ]; then
    echo "A live run sends prompts to the Anthropic API (model: ${REVERSIBLE_MODEL:-claude-sonnet-5-5}) through the model gateway,"
    echo "which holds the key and stops the run at ${GATEWAY_MAX_CALLS:-15} calls or ${GATEWAY_MAX_OUTPUT_TOKENS:-150000} output tokens. The code Claude"
    echo "writes runs in a sandbox container with no key, no token, no cluster credentials and no route out."
    if [ "$demo" = incidents ]; then
      echo "Expect 3 to 9 calls (3 builds, up to 3 attempts each)."
    elif [ "$demo" = bookings ]; then
      echo "Expect 2 to 6 calls: Claude writes the behaviours (logic stated in words) for 2 builds, up to 3 attempts each."
    else
      echo "Expect 5 to 12 calls (2 spec drafts, 3 builds of up to 3 attempts each)."
    fi
    printf 'Continue? [y/N] '
    read -r answer
    [ "$answer" = y ] || [ "$answer" = Y ] || exit 1
  fi
  gateway live
  run_demo "$demo-live" live model "$demo" secretRef gateway-token "$(gateway_url)"
}

open_() {
  echo "http://localhost:${LOCAL_PORT:-8080}/_factory/  (Ctrl-C to stop forwarding)"
  exec kubectl --context "$ctx" -n "$ns" port-forward svc/demo "${LOCAL_PORT:-8080}:8080"
}

observe() {
  echo "── Prometheus and Grafana in namespace observe (scraping the demo's /metrics)"
  kubectl --context "$ctx" apply -f "$here/manifests/observe.yaml" >/dev/null
  kubectl --context "$ctx" -n observe create configmap grafana-dashboard --from-file=dashboard.json="$here/observe/dashboard.json" \
    --dry-run=client -o yaml | kubectl --context "$ctx" apply -f - >/dev/null
  kubectl --context "$ctx" -n observe rollout restart deployment/grafana >/dev/null
  kubectl --context "$ctx" -n observe rollout status deployment/prometheus --timeout=300s
  kubectl --context "$ctx" -n observe rollout status deployment/grafana --timeout=300s
  echo "Ready. $0 dashboards, then http://localhost:3000 (the dashboard is the home page)"
}

dashboards() {
  echo "Grafana http://localhost:3000   Prometheus http://localhost:9090   (Ctrl-C to stop forwarding)"
  kubectl --context "$ctx" -n observe port-forward svc/grafana 3000:3000 &
  local g=$!
  kubectl --context "$ctx" -n observe port-forward svc/prometheus 9090:9090 &
  local p=$!
  trap 'kill $g $p 2>/dev/null' EXIT INT TERM
  wait
}

logs() { k logs --tail=-1 -c factory -l app.kubernetes.io/name=reversible-demo; }

status() {
  k get jobs,pods -l app.kubernetes.io/name=reversible-demo
  k get secret anthropic >/dev/null 2>&1 && echo "key: stored (read only by the model gateway)" || echo "key: not stored"
  k logs deploy/model-gateway --tail=3 2>/dev/null | sed 's/^/gateway: /' || true
}

# Remove the key, and restart the gateway without it (it held the key in memory).
forget_key() {
  k delete secret anthropic --ignore-not-found
  gateway rehearsal
}

down() { kind delete cluster --name "$cluster"; }

cmd=${1:-}
shift || true
case "$cmd" in
  up) up ;;
  image) image; k rollout restart deployment/fakeclaude >/dev/null ;;
  check) check ;;
  offline) offline "$@" ;;
  rehearse) rehearse "$@" ;;
  key) key ;;
  live) live "$@" ;;
  open) open_ ;;
  observe) observe ;;
  dashboards) dashboards ;;
  logs) logs ;;
  status) status ;;
  forget-key) forget_key ;;
  down) down ;;
  *) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
