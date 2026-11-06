#!/usr/bin/env bash
# gVisor (runsc) in a kind node and a RuntimeClass `gvisor`, so the sandbox's run Pods can use it
# (IDP_SANDBOX_RUNTIME_CLASS=gvisor). No root on the host: the node is a privileged container.
#   bash tests/kind/gvisor.sh                                   install and register
#   kubectl -n idp-sandbox set env deploy/idp-sandbox IDP_SANDBOX_RUNTIME_CLASS=gvisor
#   EXPECT_GVISOR=1 python3 tests/kind/check.py                 the cluster checks, expecting gVisor's kernel
# Findings on WSL2 (kernel 6.18, kind v0.31): release 20261102.0 failed to start sandboxes with the default
# `systrap` platform ("creating process: out of memory"; 20260126.0 with systrap: "failed to create a syscall
# thread"); the `kvm` and `ptrace` platforms work, `kvm` is used (/dev/kvm exists in WSL2). The inotify limits of
# the node were raised too (the default 128 instances is too few for the shim).
set -euo pipefail
NODE="${KIND_NODE:-kind-control-plane}" VERSION="${GVISOR_VERSION:-20260126.0}" PLATFORM="${GVISOR_PLATFORM:-kvm}"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
U="https://storage.googleapis.com/gvisor/releases/release/$VERSION/x86_64"
for f in runsc containerd-shim-runsc-v1; do curl -sfL -o "$T/$f" "$U/$f" && chmod +x "$T/$f"; docker cp "$T/$f" "$NODE:/usr/local/bin/$f"; done
docker exec "$NODE" sh -c "sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=1048576 >/dev/null
grep -q 'runtimes.runsc\\]' /etc/containerd/config.toml || cat >> /etc/containerd/config.toml <<'TOML'

[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]
  runtime_type = \"io.containerd.runsc.v1\"
[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc.options]
  TypeUrl = \"io.containerd.runsc.v1.options\"
  ConfigPath = \"/etc/containerd/runsc.toml\"
TOML
printf '[runsc_config]\n  platform = \"$PLATFORM\"\n' > /etc/containerd/runsc.toml
systemctl restart containerd"
kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: gvisor}
handler: runsc
YAML
echo "OK: gVisor $VERSION ($PLATFORM platform) in $NODE, RuntimeClass gvisor"
