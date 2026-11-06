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
# kind runs kubelet and containerd with the systemd cgroup driver, so runsc needs `systemd-cgroup` as well: without
# it runsc took the pod's cgroup path for a directory name and ran the sandbox outside the pod's cgroup, and the
# run's memory and CPU limits did not apply (a 256 MiB pod allocated 2 GiB). kubelet turns swap off per container,
# but runsc runs the whole sandbox in the pause container's cgroup, which keeps the swap of the WSL2 host: swap is
# turned off for all pods (kubelet-kubepods.slice), or a memory bomb spills into swap instead of being killed.
# The inotify limits and the swap setting are lost when WSL restarts: run this again.
set -euo pipefail
NODE="${KIND_NODE:-kind-control-plane}" VERSION="${GVISOR_VERSION:-20260126.0}" PLATFORM="${GVISOR_PLATFORM:-kvm}"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
U="https://storage.googleapis.com/gvisor/releases/release/$VERSION/x86_64"
for f in runsc containerd-shim-runsc-v1 runsc.sha512 containerd-shim-runsc-v1.sha512; do curl -sfL --retry 3 --retry-all-errors -o "$T/$f" "$U/$f"; done
(cd "$T" && sha512sum -c --quiet runsc.sha512 containerd-shim-runsc-v1.sha512)
for f in runsc containerd-shim-runsc-v1; do chmod +x "$T/$f"; docker cp "$T/$f" "$NODE:/usr/local/bin/$f"; done
docker exec "$NODE" sh -c "sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=1048576 >/dev/null
grep -q 'runtimes.runsc\\]' /etc/containerd/config.toml || cat >> /etc/containerd/config.toml <<'TOML'

[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc]
  runtime_type = \"io.containerd.runsc.v1\"
[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runsc.options]
  TypeUrl = \"io.containerd.runsc.v1.options\"
  ConfigPath = \"/etc/containerd/runsc.toml\"
TOML
printf '[runsc_config]\n  platform = \"$PLATFORM\"\n  systemd-cgroup = \"true\"\n' > /etc/containerd/runsc.toml
systemctl set-property kubelet-kubepods.slice MemorySwapMax=0
systemctl restart containerd"
kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: gvisor}
handler: runsc
YAML
echo "OK: gVisor $VERSION ($PLATFORM platform) in $NODE, RuntimeClass gvisor"
