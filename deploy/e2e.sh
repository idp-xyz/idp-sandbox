#!/usr/bin/env bash
# idp-sandbox behind the real idp-mcp gateway: idp-mcp's compose topology (from the side-by-side idp-mcp
# checkout, image idp-mcp:dev) plus the sandbox as an upstream. The test registers the sandbox, has it
# reviewed, grants its tools, deploys a release and then uses the sandbox only through the gateway.
#   bash deploy/e2e.sh        KEEP=1 leaves the topology running; SKIP_BUILD=1 reuses idp-mcp:dev and idp-sandbox:dev
# Next to a topology that already holds the ports and the :dev tags (the platform's compose): IDP_IMAGE and
# IDP_SANDBOX_IMAGE name other tags, IDP_E2E_COMPOSE_EXTRA is one more compose file (for example ports: !override),
# IDP_E2E_REGISTRY_URL and IDP_E2E_GATEWAY_URL say where the published ports then are.
set -euo pipefail
cd "$(dirname "$0")/.."
SBX="$PWD"
MCP="${IDP_MCP_DIR:-$SBX/../idp-mcp}"
[[ -f "$MCP/deploy/compose.yaml" ]] || { echo "BLOCKED: idp-mcp is not checked out at $MCP" >&2; exit 2; }
mkdir -p .local && umask 077
ENVF="$SBX/.local/compose.env"
if [[ ! -f "$ENVF" ]]; then
  { echo "IDP_PG_OWNER_PASSWORD=$(openssl rand -hex 16)"; echo "IDP_APP_DB_PASSWORD=$(openssl rand -hex 16)"
    echo "IDP_DEV_ADMIN_SECRET=$(openssl rand -hex 32)"; } > "$ENVF"
fi
sed -i '/^IDP_GATEWAY_ID=/d; /^IDP_GATEWAY_TOKEN=/d; /^IDP_CALLER_KEYS=/d' "$ENVF"
# the gateway's key for the caller it forwards (idp-mcp D44) and its public half for the sandbox (D18); a throw-away
# pair per run, readable by the containers' users
KEYS="$SBX/.local/caller-keys"; mkdir -p "$KEYS"
openssl ecparam -name prime256v1 -genkey -noout -out "$KEYS/gateway.key" 2>/dev/null
openssl ec -in "$KEYS/gateway.key" -pubout -out "$KEYS/gateway.pub" 2>/dev/null
# the key the sandbox and its egress proxy share (D21): used with --profile egress, made every time so the mount has a source
openssl rand -hex 32 > "$KEYS/egress.key"
chmod 755 "$KEYS"; chmod 644 "$KEYS/gateway.key" "$KEYS/gateway.pub" "$KEYS/egress.key"
echo "IDP_CALLER_KEYS=$KEYS" >> "$ENVF"
set -a; source "$ENVF"; set +a
export IDP_IMAGE="${IDP_IMAGE:-idp-mcp:dev}" IDP_SANDBOX_IMAGE="${IDP_SANDBOX_IMAGE:-idp-sandbox:dev}"
C=(docker compose -f "$MCP/deploy/compose.yaml" -f "$SBX/deploy/compose.override.yaml" ${IDP_E2E_COMPOSE_EXTRA:+-f "$IDP_E2E_COMPOSE_EXTRA"} --project-directory "$MCP" --env-file "$ENVF")
if [[ "${SKIP_BUILD:-0}" != 1 ]]; then
  (cd "$MCP" && bash deploy/build.sh "$IDP_IMAGE")
  bash deploy/build.sh "$IDP_SANDBOX_IMAGE"
fi
for img in "$IDP_IMAGE" "$IDP_SANDBOX_IMAGE" "${IDP_FIXTURE_IMAGE:-idp-fixture-upstream:local}"; do
  docker image inspect "$img" >/dev/null 2>&1 || { echo "BLOCKED: image $img missing" >&2; exit 2; }
done
"${C[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
cleanup() { [[ "${KEEP:-0}" == 1 ]] || { "${C[@]}" down -v --remove-orphans >/dev/null 2>&1; docker ps -aq --filter label=idp.sandbox=e2e | xargs -r docker rm -fv >/dev/null; docker volume ls -q --filter label=idp.sandbox=e2e | xargs -r docker volume rm -f >/dev/null; }; }
trap cleanup EXIT
# the third-party images: PostgreSQL (named by the compose file) and the runtime images the sandbox starts and the checks below run
# (the sandbox never pulls them itself), pulled here with retries so that neither compose nor docker run has to
mapfile -t THIRD < <("${C[@]}" config --images postgres)
bash deploy/pull-image.sh "${THIRD[@]}" "${IDP_SANDBOX_PYTHON_IMAGE:-public.ecr.aws/docker/library/python:3.12-slim}" "${IDP_SANDBOX_SHELL_IMAGE:-public.ecr.aws/docker/library/alpine:3.22}"
"${C[@]}" up -d --wait postgres sandbox
"${C[@]}" run --rm -T registry-admin migrate
"${C[@]}" run --rm -T registry-admin ensure-login idp_mcp_svc
TENANT="$("${C[@]}" run --rm -T registry-admin seed-tenant acme | sed -n 's/^TENANT_ID=//p')"
[[ -n "$TENANT" ]] || { echo "BLOCKED: no tenant" >&2; exit 2; }
"${C[@]}" up -d registry
CURL=(curl -sf --noproxy '*')
REG="${IDP_E2E_REGISTRY_URL:-http://127.0.0.1:8250}" GWURL="${IDP_E2E_GATEWAY_URL:-http://127.0.0.1:3320}"
for _ in $(seq 1 60); do "${CURL[@]}" -o /dev/null "$REG/api/v1/health" && break; sleep 0.5; done
tok() { "${C[@]}" run --rm -T registry-admin dev-token "$TENANT" "$1" "$2"; }
ADMIN="$(tok carol admin)"
GW="$("${CURL[@]}" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' -d '{"name":"edge-1","authMode":"api-key"}' "$REG/api/v1/gateways")"
GW_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["gateway"]["gatewayId"])' <<<"$GW")"
GW_TOKEN="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])' <<<"$GW")"
printf 'IDP_GATEWAY_ID=%s\nIDP_GATEWAY_TOKEN=%s\n' "$GW_ID" "$GW_TOKEN" >> "$ENVF"
"${C[@]}" up -d mcp-gateway
for _ in $(seq 1 60); do "${CURL[@]}" -o /dev/null "$GWURL/healthz" && break; sleep 0.5; done
set +e
IDP_E2E_REGISTRY="$REG" IDP_E2E_GATEWAY="$GWURL" IDP_E2E_GATEWAY_ID="$GW_ID" IDP_E2E_ADMIN="$ADMIN" \
IDP_E2E_OPERATOR="$(tok alice operator)" IDP_E2E_REVIEWER="$(tok bob reviewer)" \
  go test -count=1 -v -run '^TestSandboxBehindTheGateway$' ./tests/e2e/
status=$?
# Someone on the sandbox's own network, not the gateway: it names a caller, and can even copy the gateway's key id, but
# cannot sign. The sandbox must refuse it before any tool runs (D18).
NET="$(docker inspect -f '{{range $k, $_ := .NetworkSettings.Networks}}{{$k}}{{end}}' "$("${C[@]}" ps -q sandbox)")"
BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sandbox_create","arguments":{},"_meta":{"io.idp.telos/caller":{"subject":"agent-host","tenant":"x","roles":["operator"]},"io.idp.telos/caller-assertion":"eyJhbGciOiJub25lIn0.e30.","io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
DIRECT="$(docker run --rm --network "$NET" "${IDP_SANDBOX_SHELL_IMAGE:-public.ecr.aws/docker/library/alpine:3.22}" \
  wget -q -O - --header 'Content-Type: application/json' --header 'MCP-Protocol-Version: 2026-07-28' --post-data "$BODY" http://sandbox:8570/mcp 2>&1 || true)"
LISTED="$(docker run --rm --network "$NET" "${IDP_SANDBOX_SHELL_IMAGE:-public.ecr.aws/docker/library/alpine:3.22}" \
  wget -q -O - --header 'Content-Type: application/json' --header 'MCP-Protocol-Version: 2026-07-28' \
  --post-data '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' http://sandbox:8570/mcp 2>&1 || true)"
if [[ "$DIRECT" == *"403"* && "$DIRECT" != *workspaceId* && "$LISTED" == *sandbox_create* ]] && "${C[@]}" logs sandbox 2>/dev/null | grep -q 'caller assertion refused'; then
  echo "PASS direct-call-without-the-gateways-signature-refused"
else
  echo "FAIL direct-call-without-the-gateways-signature-refused: $DIRECT / ${LISTED:0:120}"; status=1
fi
set -e
[[ $status == 0 ]] || "${C[@]}" logs --tail 40 sandbox mcp-gateway registry 2>/dev/null
exit $status
