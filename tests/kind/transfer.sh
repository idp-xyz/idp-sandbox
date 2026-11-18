#!/usr/bin/env bash
# Files of any content through the Kubernetes backend on a real cluster (ADR-0001 D19): tests/kube runs the backend as
# the service account of deploy/kubernetes, with its Role and the quota of the runs namespace, and moves files of up to
# 25 MiB in (ConfigMaps) and out (pod logs). Needs the cluster kubectl points at to have deploy/kubernetes applied
# (tests/kind/smoke.sh does that; APPLY=1 applies the namespaces, the Role and the quota only) and the helper image on
# its nodes (LOAD_LOCAL_IMAGES=1 loads alpine:3.22 from the local Docker daemon into the kind cluster KIND_CLUSTER).
#   bash tests/kind/transfer.sh                                       after smoke.sh, on the same cluster
#   IDP_SANDBOX_RUNTIME_CLASS=gvisor bash tests/kind/transfer.sh      the same under gVisor (tests/kind/gvisor.sh) or kata
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER="${KIND_CLUSTER:-kind}"
if [[ "${APPLY:-0}" == 1 ]]; then
  for f in namespace rbac runs-limits; do kubectl apply -f "deploy/kubernetes/$f.yaml" >/dev/null; done
fi
if [[ "${LOAD_LOCAL_IMAGES:-0}" == 1 ]]; then
  tar="$(mktemp --suffix=.tar)"
  docker image inspect alpine:3.22 >/dev/null 2>&1 || docker tag public.ecr.aws/docker/library/alpine:3.22 alpine:3.22
  docker save --platform linux/amd64 alpine:3.22 -o "$tar" 2>/dev/null || docker save alpine:3.22 -o "$tar"
  kind load image-archive "$tar" --name "$CLUSTER" >/dev/null; rm -f "$tar"
fi
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
umask 077
kubectl -n idp-sandbox create token idp-sandbox --duration 1h > "$T/token"
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$T/ca.crt"
API="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
status=0
NO_PROXY="${NO_PROXY:-},127.0.0.1,localhost" no_proxy="${no_proxy:-},127.0.0.1,localhost" \
IDP_SANDBOX_KUBE_API="$API" IDP_SANDBOX_KUBE_TOKEN_FILE="$T/token" IDP_SANDBOX_KUBE_CA_FILE="$T/ca.crt" IDP_SANDBOX_NAMESPACE=idp-sandbox-runs \
  go test -count=1 -timeout 20m -v -run '^TestBinaryFilesOnARealCluster$' ./tests/kube/ || status=$?
# nothing of the test is left in the runs namespace: no Job, no pod, no ConfigMap, no claim
LEFT="$(kubectl -n idp-sandbox-runs get jobs,pods,configmaps,pvc -l idp.sandbox=transfer-test -o name 2>/dev/null | wc -l)"
[[ "$LEFT" == 0 ]] || { echo "FAIL: $LEFT objects of the transfer test are left in idp-sandbox-runs"; kubectl -n idp-sandbox-runs get jobs,pods,configmaps,pvc -l idp.sandbox=transfer-test; status=1; }
[[ $status == 0 ]] && echo "PASS: files of any content, up to 25 MiB, in and out of a workspace on the Kubernetes backend"
exit $status
