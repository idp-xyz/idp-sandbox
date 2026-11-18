#!/usr/bin/env bash
# The Docker backend's isolation suite with every run (and every file operation) under gVisor, without root on the
# host: a docker-in-docker daemon with the runsc runtime registered, in a privileged container; the suite talks to it
# over tcp. Needs the docker:dind image (on a machine whose daemon cannot reach Docker Hub:
# `crane pull --format=tarball docker:dind f.tar && docker load -i f.tar`) and the runsc binary (RUNSC=path or
# downloaded). `runsc` cannot apply cgroup limits in a nested daemon (it then needs --ignore-cgroups), so the memory and
# process limits cannot hold here and the wall-clock timeout contains the bombs (IDP_SANDBOX_NESTED=1 makes the suite
# insist on containment instead of on the limit). On a real node (cgroups available) leave NESTED unset.
set -euo pipefail
cd "$(dirname "$0")/../.."
VERSION="${GVISOR_VERSION:-20260126.0}" PLATFORM="${GVISOR_PLATFORM:-kvm}" PORT="${DIND_PORT:-12375}"
# DOCKER_HOST= : by the time the trap runs, docker points at the daemon inside sbx-dind, which has no such container
T="$(mktemp -d)"; trap 'rm -rf "$T"; DOCKER_HOST= docker rm -f sbx-dind >/dev/null 2>&1 || true' EXIT
RUNSC="${RUNSC:-$T/runsc}"
if [[ ! -x "$RUNSC" ]]; then   # the release's own checksum, like tests/kind/gvisor.sh
  U="https://storage.googleapis.com/gvisor/releases/release/$VERSION/x86_64"
  for f in runsc runsc.sha512; do curl -sfL --retry 3 --retry-all-errors -o "$T/$f" "$U/$f"; done
  (cd "$T" && sha512sum -c --quiet runsc.sha512) && chmod +x "$T/runsc"
fi
mkdir -p "$T/seed"
ARGS="\"--platform=$PLATFORM\""; NESTED=""
[[ "${CGROUPS:-1}" == 0 ]] && { ARGS="$ARGS,\"--ignore-cgroups\""; NESTED=1; }
printf '{"runtimes":{"runsc":{"path":"/usr/local/bin/runsc","runtimeArgs":[%s]}},"storage-driver":"overlay2"}\n' "$ARGS" > "$T/seed/daemon.json"
# docker:dind: from the daemon if it can pull, else with crane through the shell's proxy (a daemon that cannot reach Docker Hub)
if ! docker image inspect docker:dind >/dev/null 2>&1; then
  docker pull -q docker:dind >/dev/null 2>&1 || { command -v crane >/dev/null || { echo "need docker:dind (docker pull, or install crane)" >&2; exit 2; }
    crane pull --format=tarball docker:dind "$T/dind.tar" && docker load -i "$T/dind.tar" >/dev/null; }
fi
docker rm -f sbx-dind >/dev/null 2>&1 || true
docker create --name sbx-dind --privileged --cgroupns=host -e DOCKER_TLS_CERTDIR= -p "127.0.0.1:$PORT:2375" --entrypoint sh docker:dind \
  -c 'mkdir -p /etc/docker && cp /seed/daemon.json /etc/docker/daemon.json && exec dockerd-entrypoint.sh' >/dev/null
docker cp "$RUNSC" sbx-dind:/usr/local/bin/runsc; docker cp "$T/seed" sbx-dind:/seed; docker start sbx-dind >/dev/null
export DOCKER_HOST="tcp://127.0.0.1:$PORT" IDP_SANDBOX_DOCKER_HOST="tcp://127.0.0.1:$PORT"
for _ in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
# the runtime images, from the outer daemon (pulled there beforehand); with IDP_SANDBOX_OFFICE_IMAGE the office profile's
# image too, so that the office and PDF tests run under gVisor as well instead of being skipped (ADR-0001 D14, D17)
for img in public.ecr.aws/docker/library/python:3.12-slim public.ecr.aws/docker/library/alpine:3.22 ${IDP_SANDBOX_OFFICE_IMAGE:-}; do
  DOCKER_HOST= docker save --platform linux/amd64 "$img" | docker load >/dev/null
done
IDP_SANDBOX_RUNTIME=runsc IDP_SANDBOX_NESTED="$NESTED" go test -count=1 -timeout 1400s -v ./tests/integration
