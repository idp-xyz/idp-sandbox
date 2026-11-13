#!/usr/bin/env bash
# Build the office run image of idp-sandbox (ADR-0001 D14): officecli pinned by version and SHA-256, then
# docker build --network none, then a smoke run under the same restrictions as a sandbox run.
#
#   bash deploy/office/build.sh [TAG]       default idp-sandbox-office:<officecli version>
#   OFFICECLI_BINARY=file  a binary already downloaded (still checked against the pinned SHA-256)
#   BASE=image             the base (default python:3.12-slim, the python run image)
#
# Then point idp-sandbox at it: IDP_SANDBOX_OFFICE_IMAGE=<TAG>. officecli is github.com/iOfficeAI/OfficeCLI (Apache-2.0).
set -euo pipefail
cd "$(dirname "$0")/../.."
VERSION=1.0.159   # published 2026-10-27; move the pin deliberately, with the checksum of the new release
SHA256=92d9aeed324d91ed2a107f7f8dc7468829b14390a66cf2bbdfb25c40beac50ea   # officecli-linux-x64 of that release
TAG="${1:-idp-sandbox-office:$VERSION}"
BASE="${BASE:-python:3.12-slim}"
BIN="${OFFICECLI_BINARY:-${IDP_SANDBOX_CACHE:-$HOME/.cache/idp-sandbox}/officecli-$VERSION-linux-x64}"
ok() { [[ -f "$1" ]] && echo "$SHA256  $1" | sha256sum -c --quiet - 2>/dev/null; }
if ! ok "$BIN"; then
  [[ -z "${OFFICECLI_BINARY:-}" ]] || { echo "office: $BIN does not have the pinned SHA-256" >&2; exit 1; }
  mkdir -p "$(dirname "$BIN")"
  URL="https://github.com/iOfficeAI/OfficeCLI/releases/download/v$VERSION/officecli-linux-x64"
  for _ in $(seq 1 30); do   # release downloads can be slow through a proxy: resume, one HTTP/1.1 stream at a time
    curl -sSL --http1.1 -C - -m 600 -o "$BIN.part" "$URL" || true
    ok "$BIN.part" && break
    sleep 5
  done
  ok "$BIN.part" || { echo "office: download of officecli $VERSION failed or does not match the pinned SHA-256 (rerun to resume)" >&2; exit 1; }
  mv "$BIN.part" "$BIN"
fi
OUT=build/office
umask 022
rm -rf "$OUT" && mkdir -p "$OUT"
cp deploy/office/Dockerfile "$OUT/"
install -m 0755 "$BIN" "$OUT/officecli"
docker build --network none -q --build-arg "BASE=$BASE" -t "$TAG" "$OUT" >/dev/null
got="$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --user 65534:65534 \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m -e HOME=/tmp "$TAG" officecli --version)"
[[ "$got" == "$VERSION" ]] || { echo "office: the image answers '$got', not $VERSION" >&2; exit 1; }
echo "OK: $TAG $(docker image inspect --format '{{.Id}}' "$TAG" | cut -c1-19)… officecli $VERSION sha256 ${SHA256:0:12}…"
