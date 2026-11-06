#!/usr/bin/env bash
# The Kubernetes backend on a real kind cluster with Calico (tests/kind/kind.yaml: kind create cluster --config
# tests/kind/kind.yaml, then install Calico; CI does both). Needs kind, kubectl, docker, go.
# Builds the image, applies deploy/kubernetes (the image and the API-server address filled in), then runs
# tests/kind/check.py through a port-forward: tools, hardening of the run pods, no network (NetworkPolicy),
# limits, caller isolation, clean-up.
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER="${KIND_CLUSTER:-kind}"
bash deploy/build.sh idp-sandbox:ci
# load an image into the cluster; a Docker with the containerd image store saves all platforms and kind's import
# then fails, so save the one platform the nodes run
load_image() {
  local tar; tar="$(mktemp --suffix=.tar)"
  docker save --platform linux/amd64 "$1" -o "$tar" 2>/dev/null || docker save "$1" -o "$tar"
  kind load image-archive "$tar" --name "$CLUSTER" >/dev/null; rm -f "$tar"
}
# the runtime images are pulled by the cluster's nodes, as in production (CI: kind load fails on multi-platform
# images). LOAD_LOCAL_IMAGES=1 (a machine whose nodes cannot reach Docker Hub): load the ones the Docker daemon
# already has, from the mirrors the other scripts use, under the names the manifests ask for.
if [[ "${LOAD_LOCAL_IMAGES:-0}" == 1 ]]; then
  for pair in public.ecr.aws/docker/library/python:3.12-slim=python:3.12-slim public.ecr.aws/docker/library/alpine:3.22=alpine:3.22; do
    docker tag "${pair%%=*}" "${pair##*=}"; load_image "${pair##*=}"
  done
fi
load_image idp-sandbox:ci
API_IP="$(kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}')"
kubectl kustomize deploy/kubernetes \
  | sed -e 's#registry.example.com/idp/idp-sandbox@sha256:0*#idp-sandbox:ci#' -e "s#203.0.113.20/32#$API_IP/32#" > .local-kind.yaml
grep -q 'idp-sandbox:ci' .local-kind.yaml || { echo "the image was not substituted" >&2; exit 1; }
kubectl apply -f .local-kind.yaml >/dev/null
trap 'kubectl -n idp-sandbox logs deploy/idp-sandbox --tail 60 || true; kubectl -n idp-sandbox-runs get all,pvc,networkpolicy || true; kill $PF 2>/dev/null || true' EXIT
kubectl -n idp-sandbox rollout status deploy/idp-sandbox --timeout=180s
kubectl -n idp-sandbox port-forward svc/idp-sandbox 8570:8570 >/dev/null 2>&1 & PF=$!
for _ in $(seq 1 30); do curl -sf -o /dev/null --noproxy '*' -X POST http://127.0.0.1:8570/mcp && break; sleep 1; done
# cluster IPs, not names: a blocked DNS lookup would hang the probe past the run's time limit
API_IP="$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}')" SANDBOX_IP="$(kubectl -n idp-sandbox get svc idp-sandbox -o jsonpath='{.spec.clusterIP}')" python3 tests/kind/check.py
