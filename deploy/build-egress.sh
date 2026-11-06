#!/usr/bin/env bash
# Build the egress proxy image offline: a static binary on the host, then docker build --network none.
# Usage: bash deploy/build-egress.sh [TAG]     (default idp-sandbox-egress:<commit or dev>)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
TAG="${1:-idp-sandbox-egress:$VERSION}"
OUT=build/egress-image
umask 022
rm -rf "$OUT" && mkdir -p "$OUT"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT/idp-sandbox-egress" ./cmd/idp-sandbox-egress
cp deploy/Dockerfile.egress "$OUT/Dockerfile"
docker build --network none -q -t "$TAG" "$OUT" >/dev/null
echo "OK: $TAG $(docker image inspect --format '{{.Id}}' "$TAG" | cut -c1-19)…"
