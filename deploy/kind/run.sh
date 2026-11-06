#!/usr/bin/env bash
# Software built from intent, running in a local kind cluster.
#
#   deploy/kind/run.sh up                # cluster, image, namespace and network rules
#   deploy/kind/run.sh check             # prove the network rules are enforced here
#   deploy/kind/run.sh offline [demo]    # deterministic coder, no network: the baseline
#   deploy/kind/run.sh rehearse          # the live path against a stand-in API: no key, no cost
#   deploy/kind/run.sh key               # store your Anthropic key in a Secret (input hidden)
#   deploy/kind/run.sh live [demo]       # Claude writes the code (incidents) or the spec and code (factory)
#   deploy/kind/run.sh open              # port-forward the running demo to localhost:8080
#   deploy/kind/run.sh logs | status | forget-key | down
#
# demo is incidents (default) or factory. Set REVERSIBLE_MODEL to choose a
# model for live runs, LOCAL_PORT for open, YES=1 to skip the live prompt.
# docs/KIND.md walks through it.
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
  echo
  echo "Ready. Next: $0 check, then $0 offline"
}

check() {
  local ok=0
  for net in none live; do
    k delete pod "isolation-$net" --ignore-not-found --wait >/dev/null
    sed "s/__NETWORK__/$net/g" "$here/manifests/isolation-check.yaml" | kubectl --context "$ctx" apply -f - >/dev/null
    k wait --for=jsonpath='{.status.phase}'=Succeeded "pod/isolation-$net" --timeout=60s >/dev/null 2>&1 || true
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

# run_demo NAME MODE NETWORK DEMO ENVKIND ENVNAME
run_demo() {
  local name=$1 mode=$2 net=$3 demo=$4 envkind=$5 envname=$6
  case "$demo" in incidents|factory) ;; *) echo "demo is incidents or factory" >&2; exit 1 ;; esac
  k delete job -l app.kubernetes.io/name=reversible-demo --ignore-not-found --wait >/dev/null
  sed -e "s/__NAME__/$name/g" -e "s/__MODE__/$mode/g" -e "s/__NETWORK__/$net/g" -e "s/__DEMO__/$demo/g" \
      -e "s/__ENVKIND__/$envkind/g" -e "s/__ENVNAME__/$envname/g" -e "s/__MODEL__/${REVERSIBLE_MODEL:-}/g" \
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
  k logs -f "job/$name" &
  local logs=$!
  while true; do
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
  if [ "$ready" = True ]; then
    echo "── It's serving. $0 open, then:"
    echo "   http://localhost:${LOCAL_PORT:-8080}/_factory/   what's live and why; spec, examples, decisions, code"
    echo "   curl -s localhost:${LOCAL_PORT:-8080}/incidents?status=open     (use it; requests are recorded as production traffic)"
  else
    echo "── job/$name ended ($phase) before serving; the story above says why."
    return 1
  fi
}

offline() { run_demo "${1:-incidents}-offline" offline none "${1:-incidents}" configMapRef offline; }

rehearse() {
  local demo=${1:-incidents}
  if [ "$demo" != incidents ]; then
    echo "The rehearsal stand-in only writes code; the factory demo also needs a model to draft specs from conversation. Rehearse incidents." >&2
    exit 1
  fi
  run_demo incidents-rehearsal rehearsal rehearsal incidents configMapRef rehearsal
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
  echo "Stored in secret/anthropic in namespace $ns (only live runs read it). $0 forget-key removes it."
}

live() {
  local demo=${1:-incidents}
  k get secret anthropic >/dev/null 2>&1 || { echo "No key stored yet: $0 key" >&2; exit 1; }
  if [ "${YES:-}" != 1 ]; then
    echo "A live run sends prompts to the Anthropic API (model: ${REVERSIBLE_MODEL:-claude-sonnet-5-5}) and builds and runs the"
    echo "code Claude writes inside the pod: no cluster credentials, read-only root, egress limited to public HTTPS."
    if [ "$demo" = incidents ]; then
      echo "Expect 3 to 9 calls (3 builds, up to 3 attempts each)."
    else
      echo "Expect 5 to 12 calls (2 spec drafts, 3 builds of up to 3 attempts each)."
    fi
    printf 'Continue? [y/N] '
    read -r answer
    [ "$answer" = y ] || [ "$answer" = Y ] || exit 1
  fi
  run_demo "$demo-live" live live "$demo" secretRef anthropic
}

open_() {
  echo "http://localhost:${LOCAL_PORT:-8080}/_factory/  (Ctrl-C to stop forwarding)"
  k port-forward svc/demo "${LOCAL_PORT:-8080}:8080"
}

logs() { k logs --tail=-1 -l app.kubernetes.io/name=reversible-demo; }

status() {
  k get jobs,pods -l app.kubernetes.io/name=reversible-demo
  k get secret anthropic >/dev/null 2>&1 && echo "key: stored" || echo "key: not stored"
}

forget_key() { k delete secret anthropic --ignore-not-found; }

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
  logs) logs ;;
  status) status ;;
  forget-key) forget_key ;;
  down) down ;;
  *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
