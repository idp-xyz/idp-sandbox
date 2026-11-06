#!/usr/bin/env bash
# Kata Containers (a virtual machine per Pod) in a kind node and a RuntimeClass `kata`, so the sandbox's run Pods can use
# it (IDP_SANDBOX_RUNTIME_CLASS=kata). No root on the host: the node is a privileged container; needs /dev/kvm.
#   bash tests/kind/kata.sh                       downloads kata-go-static-$KATA_VERSION (1.3 GB, about 10 minutes through a proxy)
#   KATA_TARBALL=path/to/kata-go-static-4.3.1-amd64.tar.zst bash tests/kind/kata.sh      use a tarball you already have
#   EXPECT_KATA=1 python3 tests/kind/check.py                                    the cluster checks
# Findings (WSL2 with nested KVM, kind v0.31, Kata 4.3.1): the Rust runtime (runtime-rs, kata-static) needs the system D-Bus
# in the node, which the kind node image does not have; the Go runtime (kata-go-static) works. QEMU crashed in the guest
# on nested virtualization (a vCPU register dump before QMP came up); Cloud Hypervisor works. sandbox_cgroup_only=false
# (the node's kubelet uses the systemd cgroup driver). Do not give the RuntimeClass a pod overhead here.
set -euo pipefail
NODE="${KIND_NODE:-kind-control-plane}" T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
KATA_VERSION="${KATA_VERSION:-4.3.1}"
if [[ -z "${KATA_TARBALL:-}" ]]; then
  KATA_TARBALL="$T/kata-go-static.tar.zst"
  curl -fL --retry 5 -C - -o "$KATA_TARBALL" "https://github.com/kata-containers/kata-containers/releases/download/$KATA_VERSION/kata-go-static-$KATA_VERSION-amd64.tar.zst"
fi
uv run -q --no-project --with zstandard python -W ignore -c "
import sys, tarfile, zstandard
with open(sys.argv[1], 'rb') as f, tarfile.open(fileobj=zstandard.ZstdDecompressor().stream_reader(f), mode='r|') as t: t.extractall(sys.argv[2])
" "$KATA_TARBALL" "$T"
docker exec "$NODE" rm -rf /opt/kata
docker cp "$T/opt/kata" "$NODE:/opt/kata"
docker exec "$NODE" sh -c '
ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2
mkdir -p /etc/kata-containers
cp /opt/kata/share/defaults/kata-containers/configuration-clh.toml /etc/kata-containers/configuration-clh.toml
sed -i "s/^sandbox_cgroup_only *=.*/sandbox_cgroup_only = false/" /etc/kata-containers/configuration-clh.toml
grep -q "runtimes.kata\]" /etc/containerd/config.toml || cat >> /etc/containerd/config.toml <<EOF2

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata]
  runtime_type = "io.containerd.kata.v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata.options]
  ConfigPath = "/etc/kata-containers/configuration-clh.toml"
EOF2
systemctl restart containerd'
kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: kata}
handler: kata
YAML
echo "OK: Kata (Cloud Hypervisor) in $NODE, RuntimeClass kata"
