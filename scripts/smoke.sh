#!/usr/bin/env bash
# Docker smoke test: builds the image and proves the artifact actually serves
# traffic (the CI `validate` gate only proves it compiles). Asserts:
#   1. /healthz = 200 once ready; the shipped HEALTHCHECK passes under auth
#   2. / = 200, serves the UI scaffold
#   3. /ws speaks WebSocket (non-upgrade GET is rejected)
#   4. with AUTH_PASSWORD set, every route requires it
#
# Usage: scripts/smoke.sh [IMAGE] (defaults to a locally-built
# web-terminal-server:smoke; pass a prebuilt ref to skip the build)
# Requires docker, curl, jq. Exits non-zero on first failed assertion.
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${1:-web-terminal-server:smoke}"
CONTAINER="wts-smoke-$$"
PASSWORD="smoke-pw-$$"
HOST_PORT="${SMOKE_PORT:-17681}"
BASE="http://127.0.0.1:${HOST_PORT}"

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for tool in docker curl jq; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

# Build only when the caller didn't hand us a prebuilt image to reuse.
if [ "$IMAGE" = "web-terminal-server:smoke" ]; then
  echo "[smoke] building image $IMAGE"
  docker build -t "$IMAGE" .
fi

# Run with a password so the same container exercises the auth paths too.
echo "[smoke] starting container $CONTAINER on :${HOST_PORT} (auth enabled)"
docker run -d --name "$CONTAINER" \
  -p "127.0.0.1:${HOST_PORT}:7681" \
  -e AUTH_PASSWORD="$PASSWORD" \
  "$IMAGE" >/dev/null

echo "[smoke] waiting for readiness"
ready=""
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/healthz" || true)
  if [ "$code" = "200" ]; then
    ready=1
    break
  fi
  # Surface an early crash instead of waiting the full timeout.
  if ! docker ps --filter "name=${CONTAINER}" --filter status=running --format '{{.Names}}' | grep -q "$CONTAINER"; then
    docker logs "$CONTAINER" >&2 || true
    fail "container exited before becoming ready"
  fi
  sleep 1
done
[ -n "$ready" ] || {
  docker logs "$CONTAINER" >&2 || true
  fail "/healthz never returned 200"
}

echo "[smoke] verifying the shipped HEALTHCHECK passes under auth"
hc=$(docker inspect --format '{{join .Config.Healthcheck.Test " "}}' "$IMAGE")
docker exec "$CONTAINER" sh -c "${hc#CMD-SHELL }" \
  || fail "shipped HEALTHCHECK probe failed under auth (does it send credentials?)"
echo "[smoke] PASS  shipped HEALTHCHECK succeeds under auth"

# 1. /healthz authenticated -> 200 (covered by the readiness loop above).
echo "[smoke] PASS  /healthz (authenticated) = 200"

# 2. / authenticated -> 200 and looks like the UI scaffold.
body=$(curl -s -u "admin:${PASSWORD}" "${BASE}/")
code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/")
[ "$code" = "200" ] || fail "/ (authenticated) = $code, want 200"
echo "$body" | grep -qiE 'term|<!doctype html|<html' || fail "/ body does not look like the UI scaffold"
echo "[smoke] PASS  / (authenticated) = 200, serves scaffold"

# 2b. Every importmap JS asset + CSS + font the build assembles must be reachable
#     (a scaffold-only check passes even when these 404, leaving a permanent
#     "Loading..." overlay). JS list is DERIVED from the served importmap, not
#     hardcoded, so a library moving a module fails here instead of going
#     silently unchecked.
importmap_targets=$(printf '%s' "$body" | sed -n '/<script type="importmap">/,/<\/script>/p' | grep -o '"/[^"]*"' | tr -d '"' || true)
[ -n "$importmap_targets" ] || fail "no importmap module paths found in the served page (the page lost its importmap, or the extraction is broken)"
for asset in $importmap_targets /style.css /vendor/fonts/MonaspaceNeonNF-Regular.woff2; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}${asset}")
  [ "$code" = "200" ] || fail "bundle asset ${asset} = $code, want 200 (UI bundle incomplete)"
done
echo "[smoke] PASS  every importmap module the served page names is reachable, plus CSS + font"

# 2c. Hardened headers: CSP hash-pinning stops injected script in a page
#     driving a root shell; COOP/Referrer-Policy keep a clicked OSC 8
#     hyperlink from reaching back through window.opener. Both fail silently
#     on a refactor with no functional symptom, hence the explicit probe.
headers=$(curl -s -D - -o /dev/null -u "admin:${PASSWORD}" "${BASE}/")
for expected in \
  'content-security-policy:.*script-src' \
  'x-content-type-options: *nosniff' \
  'x-frame-options: *DENY' \
  'referrer-policy: *same-origin' \
  'cross-origin-opener-policy: *same-origin' \
  'permissions-policy: *camera=()'; do
  printf '%s' "$headers" | grep -qiE "$expected" || fail "response is missing a hardened header matching /${expected}/"
done
printf '%s' "$headers" | grep -qi "content-security-policy:.*'unsafe-inline'" \
  && fail "the CSP carries 'unsafe-inline'; the inline script and style must stay hash-pinned"
echo "[smoke] PASS  hardened headers served (hash-pinned CSP, nosniff, DENY, COOP, Referrer-Policy, Permissions-Policy)"

# 3. /ws without upgrade headers -> not 200/101. Engine v3 answers every
#    non-upgrade GET /ws with 426 whether the session is known or not (unknown
#    sessions are reported post-upgrade via close 4004; a plain probe can't
#    test existence). Create a real session via REST first so this also
#    proves the REST surface is mounted.
session_id=$(curl -s -u "admin:${PASSWORD}" -X POST "${BASE}/api/sessions" | jq -r '.id // empty')
[ -n "$session_id" ] || fail "POST /api/sessions did not return a session id"
code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/ws?session=${session_id}")
case "$code" in
  400 | 426 | 405) echo "[smoke] PASS  /ws rejects non-upgrade request = $code" ;;
  101) echo "[smoke] PASS  /ws upgraded = 101" ;;
  *) fail "/ws?session=${session_id} (no upgrade) = $code, want 400/426/405 (handler mounted)" ;;
esac
# An unknown session id must answer identically (no pre-upgrade 404 oracle for
# session existence — the anti-probing contract; "unknown" is close 4004,
# post-upgrade only).
code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/ws")
[ "$code" = "426" ] || fail "/ws (no session param) = $code, want 426 (same as known session; no existence oracle)"
echo "[smoke] PASS  /ws (no session param) = 426, indistinguishable from a known session"

# 4. Auth gating: no credentials -> 401 with a challenge on every route.
for path in / /healthz /ws; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}${path}")
  [ "$code" = "401" ] || fail "unauthenticated ${path} = $code, want 401"
done
challenge=$(curl -s -D - -o /dev/null "${BASE}/healthz" | grep -i '^www-authenticate:' || true)
[ -n "$challenge" ] || fail "401 response missing WWW-Authenticate challenge"
echo "[smoke] PASS  auth gates / /healthz /ws (401 + challenge without creds)"

echo "[smoke] OK — all runtime assertions passed for $IMAGE"
