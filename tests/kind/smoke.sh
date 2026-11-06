#!/usr/bin/env bash
# The Kubernetes backend on a real kind cluster (needs kind, kubectl, docker, go; CI: .github/workflows/ci.yml).
# Builds the image, applies deploy/kubernetes (the image and the API-server address filled in), then runs
# tests/kind/check.py through a port-forward: tools, hardening of the run pods, no network (NetworkPolicy),
# limits, caller isolation, clean-up.
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER="${KIND_CLUSTER:-kind}"
bash deploy/build.sh idp-sandbox:ci
for img in python:3.12-slim alpine:3.22; do docker pull -q "$img" >/dev/null; kind load docker-image "$img" --name "$CLUSTER" >/dev/null; done
kind load docker-image idp-sandbox:ci --name "$CLUSTER" >/dev/null
API_IP="$(kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}')"
kubectl kustomize deploy/kubernetes \
  | sed -e 's#registry.example.com/idp/idp-sandbox@sha256:0*#idp-sandbox:ci#' -e "s#203.0.113.20/32#$API_IP/32#" > .local-kind.yaml
grep -q 'idp-sandbox:ci' .local-kind.yaml || { echo "the image was not substituted" >&2; exit 1; }
kubectl apply -f .local-kind.yaml >/dev/null
trap 'kubectl -n idp-sandbox logs deploy/idp-sandbox --tail 60 || true; kubectl -n idp-sandbox-runs get all,pvc,networkpolicy || true; kill $PF 2>/dev/null || true' EXIT
kubectl -n idp-sandbox rollout status deploy/idp-sandbox --timeout=180s
kubectl -n idp-sandbox port-forward svc/idp-sandbox 8570:8570 >/dev/null 2>&1 & PF=$!
for _ in $(seq 1 30); do curl -sf -o /dev/null --noproxy '*' -X POST http://127.0.0.1:8570/mcp && break; sleep 1; done
python3 -I tests/kind/check.py
