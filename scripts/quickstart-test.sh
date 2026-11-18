#!/usr/bin/env bash
# E2E quickstart: a throwaway kind cluster, the agent chart with a locally built image, and the CLI report.
# Uses its own kubeconfig file, so your current kube context and the minikube profile are never touched.
set -euo pipefail
cd "$(dirname "$0")/.."

name=${KIND_CLUSTER:-phrontis-quickstart}
for tool in kind helm docker kubectl go; do
  command -v "$tool" >/dev/null || { echo "quickstart-test: '$tool' is not installed; install it and rerun" >&2; exit 2; }
done
docker info >/dev/null 2>&1 || { echo "quickstart-test: docker is not running" >&2; exit 2; }
if kind get clusters 2>/dev/null | grep -qx "$name"; then
  echo "quickstart-test: kind cluster '$name' already exists; delete it or set KIND_CLUSTER" >&2
  exit 2
fi

tmp=$(mktemp -d)
export KUBECONFIG=$tmp/kubeconfig
cleanup() { kind delete cluster --name "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

echo "== build image"
docker build -q -t phrontis-agent:quickstart .

echo "== create kind cluster $name"
kind create cluster --name "$name" --kubeconfig "$KUBECONFIG" --wait 120s
kind load docker-image phrontis-agent:quickstart --name "$name"

echo "== install the agent chart"
# The API does not exist here: the agent only has to start and stay up (it logs failed posts and keeps going).
helm install phrontis-agent deploy/helm/phrontis-agent -n phrontis --create-namespace \
  --set image.repository=phrontis-agent --set image.tag=quickstart --set image.pullPolicy=Never \
  --set api.url=http://127.0.0.1:1 --set api.key=quickstart
kubectl -n phrontis rollout status deploy/phrontis-agent --timeout=120s

echo "== read-only check: the agent's ClusterRole has only get/list/watch"
if kubectl get clusterrole phrontis-phrontis-agent -o jsonpath='{.rules[*].verbs[*]}' | tr ' ' '\n' | grep -vxE 'get|list|watch' | grep -q .; then
  echo "FAIL: the agent ClusterRole grants more than get/list/watch" >&2
  exit 1
fi

echo "== report"
# No metrics-server on a clean kind: costs by request only, which is what the README promises.
report=$(go run ./cmd/phrontis --cloud-prices=false)
echo "$report"
if ! grep -q 'kube-system' <<<"$report"; then
  echo "FAIL: the report is empty or has no kube-system namespace" >&2
  exit 1
fi
echo "quickstart-test OK"
