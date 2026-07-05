#!/usr/bin/env bash
# Docker smoke test: build the image and prove the real artifact actually
# serves traffic — not just that it compiles (the CI `validate` gate already
# checks build-ability). Asserts the runtime contract:
#
#   1. /healthz returns 200 once the container is ready (the HEALTHCHECK
#      target), and the image's own shipped HEALTHCHECK probe -- run via
#      `docker exec` -- passes under auth, proving the embedded probe sends
#      credentials
#   2. /          returns 200 and serves the UI scaffold (embedded static FS)
#   3. /ws        speaks WebSocket (a plain GET without upgrade headers is
#                 rejected, proving the engine handler is mounted)
#   4. with WT_PASSWORD set, every route (incl. /healthz and /ws) returns 401
#      without credentials and 200 with them (Basic-auth middleware is wired)
#
# Usage:  scripts/smoke.sh [IMAGE]
#   IMAGE defaults to a locally-built `web-terminal-server:smoke`. Pass an
#   already-built/pulled image ref to skip the build (CI builds once, then
#   reuses the loaded image).
#
# Requires docker, curl, and jq. Exits non-zero on the first failed assertion and
# always removes the container it started.
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

# Run with a password so the same container exercises the auth paths too. Use a
# short-lived command; the server itself is what we probe, not the PTY.
echo "[smoke] starting container $CONTAINER on :${HOST_PORT} (auth enabled)"
docker run -d --name "$CONTAINER" \
  -p "127.0.0.1:${HOST_PORT}:7681" \
  -e WT_PASSWORD="$PASSWORD" \
  "$IMAGE" >/dev/null

# Wait for readiness via the authenticated /healthz (max ~30s).
echo "[smoke] waiting for readiness"
ready=""
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/healthz" || true)
  if [ "$code" = "200" ]; then
    ready=1
    break
  fi
  # Surface an early container crash instead of waiting the full timeout.
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

# 2b. The scaffold references importmap JS + CSS + font assets the build
#     assembles into static/vendor/ and static/style.css. A scaffold-only
#     check passes even when those 404, leaving the user on a permanent
#     "Loading..." overlay (mount() throws on the failed module import).
for asset in \
  /style.css \
  /vendor/cplieger-web-terminal-ui/index.js \
  /vendor/cplieger-web-terminal-ui/presets.js \
  /vendor/cplieger-web-terminal-engine/index.js \
  /vendor/fonts/MonaspiceNeNerdFontMono-Regular.otf; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}${asset}")
  [ "$code" = "200" ] || fail "bundle asset ${asset} = $code, want 200 (UI bundle incomplete)"
done
echo "[smoke] PASS  importmap-referenced bundle assets served (CSS, engine+UI JS, font)"

# 3. /ws authenticated but WITHOUT upgrade headers -> not a 200/101. The engine
#    (v2, multi-session) 404s /ws for an unknown/missing ?session=<id> before
#    it ever looks at upgrade headers, so a bare GET /ws can't distinguish "no
#    session" from "handler not mounted." Create a real session via the REST
#    API first so the request reaches the WebSocket handler itself, which must
#    then reject the non-upgrade request (typically 400/426).
session_id=$(curl -s -u "admin:${PASSWORD}" -X POST "${BASE}/api/sessions" | jq -r '.id // empty')
[ -n "$session_id" ] || fail "POST /api/sessions did not return a session id"
code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/ws?session=${session_id}")
case "$code" in
  400 | 426 | 405) echo "[smoke] PASS  /ws rejects non-upgrade request = $code" ;;
  101) echo "[smoke] PASS  /ws upgraded = 101" ;;
  *) fail "/ws?session=${session_id} (no upgrade) = $code, want 400/426/405 (handler mounted)" ;;
esac
# An unknown/missing session id is a distinct, documented 404 (SessionManager.
# WebSocketHandler) -- verify that contract too so a regression there is caught.
code=$(curl -s -o /dev/null -w '%{http_code}' -u "admin:${PASSWORD}" "${BASE}/ws")
[ "$code" = "404" ] || fail "/ws (no session param) = $code, want 404 (unknown session)"
echo "[smoke] PASS  /ws (no session param) = 404"

# 4. Auth gating: no credentials -> 401 with a challenge on every route.
for path in / /healthz /ws; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}${path}")
  [ "$code" = "401" ] || fail "unauthenticated ${path} = $code, want 401"
done
challenge=$(curl -s -D - -o /dev/null "${BASE}/healthz" | grep -i '^www-authenticate:' || true)
[ -n "$challenge" ] || fail "401 response missing WWW-Authenticate challenge"
echo "[smoke] PASS  auth gates / /healthz /ws (401 + challenge without creds)"

echo "[smoke] OK — all runtime assertions passed for $IMAGE"
