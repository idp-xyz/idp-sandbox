#!/usr/bin/env bash
# Egress on the Kubernetes backend on a real kind cluster with Calico (ADR-0001 D22). Needs kind, kubectl, docker, go, and
# a cluster whose CNI enforces NetworkPolicies (tests/kind/kind.yaml plus Calico; CI and tests/kind/local-cluster.sh).
# Builds the service and the proxy, applies deploy/kubernetes-egress with a test allow-list and two in-cluster origins,
# runs tests/kind/egress.py through a port-forward, and then shows that the service refuses to start when a policy lets
# egress runs reach more than the proxy. It removes its own namespace (idp-sandbox-egress-test) and policies, and leaves
# the overlay applied with the test's allow-lists and the proxy still accepting private addresses: a cluster for tests.
#   bash tests/kind/egress.sh                          after or instead of smoke.sh, on the same cluster
#   LOAD_LOCAL_IMAGES=1 bash tests/kind/egress.sh      nodes that cannot pull python:3.12-slim and alpine:3.22
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER="${KIND_CLUSTER:-kind}"
TEST=idp-sandbox-egress-test
bash deploy/build.sh idp-sandbox:ci
bash deploy/build-egress.sh idp-sandbox-egress:ci
load_image() {   # one platform: kind's import fails on what a containerd image store saves otherwise
  local tar; tar="$(mktemp --suffix=.tar)"
  docker save --platform linux/amd64 "$1" -o "$tar" 2>/dev/null || docker save "$1" -o "$tar"
  kind load image-archive "$tar" --name "$CLUSTER" >/dev/null; rm -f "$tar"
}
if [[ "${LOAD_LOCAL_IMAGES:-0}" == 1 ]]; then
  for pair in ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__python__3.12-slim=ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__python__3.12-slim ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__alpine__3.22=ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__alpine__3.22; do
    docker tag "${pair%%=*}" "${pair##*=}"; load_image "${pair##*=}"
  done
fi
load_image idp-sandbox:ci
load_image idp-sandbox-egress:ci

T="$(mktemp -d)"
PF=""
cleanup() {
  [[ -z "$PF" ]] || kill "$PF" 2>/dev/null || true
  kubectl -n idp-sandbox-runs delete networkpolicy egress-test-too-wide --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete namespace "$TEST" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n idp-sandbox delete networkpolicy egress-proxy-test-origins --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$T"
}
diagnose() {
  kubectl -n idp-sandbox logs deploy/idp-sandbox --tail 40 || true
  kubectl -n idp-sandbox logs deploy/idp-sandbox-egress --tail 40 || true
  kubectl -n idp-sandbox get pods,networkpolicy -o wide || true
  kubectl -n idp-sandbox-runs get all,pvc,networkpolicy || true
}
trap 'status=$?; [[ $status == 0 ]] || diagnose; cleanup' EXIT

API_IP="$(kubectl get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}')"
API_SVC_IP="$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}')"
# the overlay as shipped, with the test's allow-lists in place of the empty one: two tenants with a host each, and one
# whose list names the API server, which the proxy's NetworkPolicy must refuse all the same
cp -r deploy/kubernetes deploy/kubernetes-egress "$T/"
cat > "$T/kubernetes-egress/tenants.yaml" <<YAML
default:
  hosts: []
tenants:
  acme:
    hosts: [acme-site.$TEST.svc.cluster.local]
  globex:
    hosts: [globex-site.$TEST.svc.cluster.local]
  probe:
    hosts: ["$API_SVC_IP"]
YAML
mkdir "$T/test"
cat > "$T/test/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../kubernetes-egress
  - origins.yaml
patches:
  - target: {kind: Deployment, name: idp-sandbox-egress}
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: IDP_EGRESS_ALLOW_PRIVATE, value: "true"}
YAML
# the origins are in the cluster, so the proxy is told to accept private addresses and one policy lets it reach them;
# the test namespace has no policy of its own: what stops a run reaching the web server directly is the run's
cat > "$T/test/origins.yaml" <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: $TEST
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: $TEST
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: web
          image: ghcr.io/idp-xyz/mirror:public.ecr.aws__docker__library__python__3.12-slim
          imagePullPolicy: IfNotPresent
          command:
            - python
            - -c
            - |
              import http.server, socketserver
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(s):
                      s.send_response(200); s.end_headers(); s.wfile.write(b'hello from the test origin')
                  def log_message(*a): pass
              socketserver.ThreadingTCPServer.allow_reuse_address = True
              socketserver.ThreadingTCPServer(('0.0.0.0', 8080), H).serve_forever()
          ports: [{containerPort: 8080}]
          readinessProbe: {tcpSocket: {port: 8080}, periodSeconds: 2}
---
apiVersion: v1
kind: Service
metadata: {name: acme-site, namespace: $TEST}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: globex-site, namespace: $TEST}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: egress-proxy-test-origins, namespace: idp-sandbox}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/component: egress}}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $TEST}}
          podSelector: {matchLabels: {app: web}}
      ports: [{protocol: TCP, port: 8080}]
YAML
kubectl kustomize "$T/test" \
  | sed -e 's#registry.example.com/idp/idp-sandbox-egress@sha256:0*#idp-sandbox-egress:ci#' \
        -e 's#registry.example.com/idp/idp-sandbox@sha256:0*#idp-sandbox:ci#' -e "s#203.0.113.20/32#$API_IP/32#" > "$T/all.yaml"
grep -q 'image: idp-sandbox:ci' "$T/all.yaml" && grep -q 'image: idp-sandbox-egress:ci' "$T/all.yaml" || { echo "the images were not substituted" >&2; exit 1; }

# a run of this script that has just ended may still be removing its namespace
kubectl wait --for=delete "namespace/$TEST" --timeout=180s >/dev/null 2>&1 || true
# the key the service and the proxy share: made here, kept nowhere else
kubectl apply -f deploy/kubernetes/namespace.yaml >/dev/null
kubectl -n idp-sandbox create secret generic idp-sandbox-egress-key --from-literal=egress.key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$T/all.yaml" >/dev/null
kubectl -n "$TEST" rollout status deploy/web --timeout=180s
# the proxy starts anew, so that its log is this run's
kubectl -n idp-sandbox rollout restart deploy/idp-sandbox-egress >/dev/null
kubectl -n idp-sandbox rollout status deploy/idp-sandbox-egress --timeout=180s
# a service that was already running (smoke.sh) has to start again to probe with the proxy up; a new one may have
# probed before the proxy was ready, which it only warns about
kubectl -n idp-sandbox rollout restart deploy/idp-sandbox >/dev/null
kubectl -n idp-sandbox rollout status deploy/idp-sandbox --timeout=240s
kubectl -n idp-sandbox port-forward svc/idp-sandbox 8570:8570 >/dev/null 2>&1 & PF=$!
# ready when a well-formed tools/list answers 200 (it needs no caller). The bodiless POST this replaces was always
# answered with 400, so curl -f never succeeded and the loop slept its full 30 seconds.
READY=""
for _ in $(seq 1 30); do
  curl -sf -o /dev/null --noproxy '*' -X POST http://127.0.0.1:8570/mcp -H 'Content-Type: application/json' -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/list' \
    --data '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' && { READY=1; break; }
  sleep 1
done
[[ -n "$READY" ]] || { echo "FAIL: idp-sandbox did not answer tools/list on 127.0.0.1:8570 within 30 seconds" >&2; exit 1; }
NO_PROXY="${NO_PROXY:-},127.0.0.1,localhost" no_proxy="${no_proxy:-},127.0.0.1,localhost" python3 tests/kind/egress.py
kill "$PF" 2>/dev/null || true; PF=""

# The start-up probe (D22), the other way round: a policy that lets egress runs out to anywhere. The service must find
# out and stay down, and come back once the policy is gone.
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: egress-test-too-wide, namespace: idp-sandbox-runs}
spec:
  podSelector: {matchLabels: {idp.sandbox/network: egress}}
  policyTypes: [Egress]
  egress: [{}]
YAML
kubectl -n idp-sandbox rollout restart deploy/idp-sandbox >/dev/null
REFUSED=0
for _ in $(seq 1 60); do
  for flag in "" "--previous"; do
    if kubectl -n idp-sandbox logs -l app.kubernetes.io/component=sandbox --tail=20 $flag 2>/dev/null | grep -q "lets them reach more than the egress proxy"; then REFUSED=1; fi
  done
  [[ $REFUSED == 1 ]] && break; sleep 2
done
READY="$(kubectl -n idp-sandbox get deploy idp-sandbox -o jsonpath='{.status.readyReplicas}')"
if [[ $REFUSED == 1 && -z "$READY" ]]; then
  echo "PASS service-refuses-to-start-when-egress-runs-reach-more-than-the-proxy"
else
  echo "FAIL service-refuses-to-start-when-egress-runs-reach-more-than-the-proxy  (refused=$REFUSED ready=$READY)"; exit 1
fi
kubectl -n idp-sandbox-runs delete networkpolicy egress-test-too-wide >/dev/null
kubectl -n idp-sandbox rollout restart deploy/idp-sandbox >/dev/null
kubectl -n idp-sandbox rollout status deploy/idp-sandbox --timeout=240s
echo "PASS: egress on the Kubernetes backend: per tenant, through the proxy only, and checked by the service at start-up"
