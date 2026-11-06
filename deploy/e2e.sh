#!/usr/bin/env bash
# idp-sandbox behind the real idp-mcp gateway: idp-mcp's compose topology (from the side-by-side idp-mcp
# checkout, image idp-mcp:dev) plus the sandbox as an upstream. The test registers the sandbox, has it
# reviewed, grants its tools, deploys a release and then uses the sandbox only through the gateway.
#   bash deploy/e2e.sh        KEEP=1 leaves the topology running; SKIP_BUILD=1 reuses idp-mcp:dev and idp-sandbox:dev
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
sed -i '/^IDP_GATEWAY_ID=/d; /^IDP_GATEWAY_TOKEN=/d' "$ENVF"
set -a; source "$ENVF"; set +a
C=(docker compose -f "$MCP/deploy/compose.yaml" -f "$SBX/deploy/compose.override.yaml" --project-directory "$MCP" --env-file "$ENVF")
if [[ "${SKIP_BUILD:-0}" != 1 ]]; then
  (cd "$MCP" && bash deploy/build.sh idp-mcp:dev)
  bash deploy/build.sh idp-sandbox:dev
fi
for img in idp-mcp:dev idp-sandbox:dev "${IDP_FIXTURE_IMAGE:-idp-fixture-upstream:local}"; do
  docker image inspect "$img" >/dev/null 2>&1 || { echo "BLOCKED: image $img missing" >&2; exit 2; }
done
"${C[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
cleanup() { [[ "${KEEP:-0}" == 1 ]] || { "${C[@]}" down -v --remove-orphans >/dev/null 2>&1; docker ps -aq --filter label=idp.sandbox=e2e | xargs -r docker rm -fv >/dev/null; docker volume ls -q --filter label=idp.sandbox=e2e | xargs -r docker volume rm -f >/dev/null; }; }
trap cleanup EXIT
"${C[@]}" up -d --wait postgres sandbox
"${C[@]}" run --rm -T registry-admin migrate
"${C[@]}" run --rm -T registry-admin ensure-login idp_mcp_svc
TENANT="$("${C[@]}" run --rm -T registry-admin seed-tenant acme | sed -n 's/^TENANT_ID=//p')"
[[ -n "$TENANT" ]] || { echo "BLOCKED: no tenant" >&2; exit 2; }
"${C[@]}" up -d registry
CURL=(curl -sf --noproxy '*')
REG=http://127.0.0.1:8250 GWURL=http://127.0.0.1:3320
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
set -e
[[ $status == 0 ]] || "${C[@]}" logs --tail 40 sandbox mcp-gateway registry 2>/dev/null
exit $status
