#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Container smoke test (testing.md L5): starts IMAGE with examples/config
# the way deploy.md runs it (read-only root filesystem and config, all
# capabilities dropped, signing key file), waits until the image's
# HEALTHCHECK reports healthy, then fetches /healthz and /playlist.m3u8.
#
# Usage, from the repository root:
#   .github/scripts/container-smoke.sh IMAGE [VERSION]
# VERSION, if given, must match the output of `wg-stream-proxy version`.
set -euo pipefail

image="${1:?usage: container-smoke.sh IMAGE [VERSION]}"
want_version="${2:-}"
name="wg-stream-proxy-smoke-$$"
tmp="$(mktemp -d)"

cleanup() {
  status=$?
  if [ "$status" -ne 0 ] && docker inspect "$name" > /dev/null 2>&1; then
    echo "--- container log"
    docker logs "$name" 2>&1 || true
    echo "--- health"
    docker inspect -f '{{json .State.Health}}' "$name" || true
  fi
  docker rm -f "$name" > /dev/null 2>&1 || true
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "--- version"
out="$(docker run --rm "$image" version)"
echo "$out"
if [ -n "$want_version" ]; then
  test "$(head -n 1 <<< "$out")" = "wg-stream-proxy ${want_version}" ||
    fail "version: want ${want_version}"
fi

user="$(docker image inspect -f '{{.Config.User}}' "$image")"
test "$user" = "65532:65532" || fail "image user: got ${user}, want 65532:65532"

# Test-only key, readable by the container user (UID 65532).
openssl rand -base64 32 > "$tmp/signing.key"
chmod 644 "$tmp/signing.key"

echo "--- start"
docker run -d --name "$name" \
  --read-only --cap-drop ALL \
  -p 127.0.0.1::2349 \
  -v "$PWD/examples/config:/etc/wg-stream-proxy:ro" \
  -v "$tmp/signing.key:/run/secrets/signing.key:ro" \
  -e WG_STREAM_PROXY_SIGNING_KEY_FILE=/run/secrets/signing.key \
  "$image" > /dev/null

echo "--- wait for HEALTHCHECK"
health=""
for _ in $(seq 60); do
  health="$(docker inspect -f '{{.State.Health.Status}}' "$name")"
  running="$(docker inspect -f '{{.State.Running}}' "$name")"
  [ "$health" = healthy ] && break
  [ "$running" = true ] || fail "container exited"
  sleep 1
done
test "$health" = healthy || fail "health: ${health}"
echo "$health"

port="$(docker port "$name" 2349/tcp | head -n 1)"
port="${port##*:}"
base="http://127.0.0.1:${port}"

echo "--- /healthz"
curl -fsS --max-time 5 "$base/healthz" | tee "$tmp/healthz"
grep -q 'github.com/KingOfKalk/wg-stream-proxy' "$tmp/healthz" ||
  fail "/healthz: no source URL"

echo "--- /playlist.m3u8"
curl -fsS --max-time 5 "$base/playlist.m3u8" | tee "$tmp/playlist"
test "$(head -n 1 "$tmp/playlist" | tr -d '\r')" = "#EXTM3U" ||
  fail "/playlist.m3u8: no #EXTM3U header"
grep -q "^${base}/c/" "$tmp/playlist" || fail "/playlist.m3u8: no channel URLs"

echo "--- ok"
