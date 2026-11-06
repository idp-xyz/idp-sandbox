#!/usr/bin/env bash
# A local kind cluster with Calico (NetworkPolicies enforced) on a machine whose Docker daemon cannot reach Docker Hub
# or quay.io but whose shell can (an https_proxy): the node and Calico images are fetched with crane through the
# proxy and loaded from tarballs. No root needed: kind, kubectl and crane go to ~/.local/bin.
#   bash tests/kind/local-cluster.sh            create "kind"
#   LOAD_LOCAL_IMAGES=1 bash tests/kind/smoke.sh  then run the Kubernetes backend checks against it
#   kind delete cluster --name kind              remove it
set -euo pipefail
cd "$(dirname "$0")/../.."
BIN="$HOME/.local/bin"; mkdir -p "$BIN"; export PATH="$BIN:$PATH"
NODE=kindest/node:v1.35.0 CALICO=v3.30.3
command -v kubectl >/dev/null || curl -sL -o "$BIN/kubectl" "https://dl.k8s.io/release/$(curl -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && chmod +x "$BIN/kubectl"
command -v kind >/dev/null || GOBIN="$BIN" go install sigs.k8s.io/kind@v0.31.0
command -v crane >/dev/null || GOBIN="$BIN" go install github.com/google/go-containerregistry/cmd/crane@latest
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
docker image inspect "$NODE" >/dev/null 2>&1 || { crane pull --format=tarball "$NODE" "$T/node.tar" && docker load -i "$T/node.tar" >/dev/null; }
kind get clusters 2>/dev/null | grep -qx kind || kind create cluster --name kind --config tests/kind/kind.yaml --image "$NODE" --wait 0s
for i in cni node kube-controllers; do crane pull "quay.io/calico/$i:$CALICO" "$T/$i.tar" && kind load image-archive "$T/$i.tar" --name kind >/dev/null; done
curl -s "https://raw.githubusercontent.com/projectcalico/calico/$CALICO/manifests/calico.yaml" | sed 's#docker.io/calico/#quay.io/calico/#' | kubectl apply -f - >/dev/null
kubectl -n kube-system rollout status ds/calico-node --timeout=300s
kubectl wait --for=condition=Ready nodes --all --timeout=300s
echo "OK: kind cluster 'kind' with Calico"
