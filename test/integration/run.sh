#!/usr/bin/env bash
# Ballast integration test harness: the first real end-to-end exercise of
# Ballast against a live Docker socket. It builds the image, brings up a
# single throwaway labeled container with known data in a volume, drives
# Ballast through a real backup, snapshot listing, and restore, diffs the
# restored data against the original, and tears everything down.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container, volume) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below, so a failed run
# does not leave test objects behind.
#
# Usage: test/integration/run.sh [--keep] [--skip-daemon]
#   --keep         skip cleanup at the end (for debugging a failure)
#   --skip-daemon  skip the daemon smoke test (step 10); just the one-shot
#                  backup -> snapshots -> restore path

set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../.." && pwd)"

KEEP=0
SKIP_DAEMON=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    --skip-daemon) SKIP_DAEMON=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

SVC=ballast-itest-svc
VOL=ballast-itest-data
DAEMON=ballast-itest-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/ballast.itest.yml"
REPOS="$HARNESS_DIR/repos"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$DAEMON" >/dev/null 2>&1 || true
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
echo "DockerRootDir: $DOCKER_ROOT"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into ballast.itest.yml (/var/lib/docker/volumes)." >&2
  echo "      Add a host_roots entry mapping $VOLUMES_ROOT to itself before running this harness." >&2
  exit 1
fi

mkdir -p "$REPOS" "$SECRETS" "$RESTORE"

# --- secrets -----------------------------------------------------------------

if [ ! -s "$SECRETS/repo-master-key" ]; then
  log "generating repo-master-key"
  openssl rand -base64 32 > "$SECRETS/repo-master-key"
fi

# --- build ---------------------------------------------------------------

log "building $IMAGE"
docker build -f "$REPO_ROOT/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

# --- test service ----------------------------------------------------------

log "creating $SVC with canary data in $VOL"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

sleep 1
ORIGINAL_CANARY="$(docker exec "$SVC" cat /data/canary.txt)"
echo "canary: $ORIGINAL_CANARY"

ballast() {
  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# --- backup ------------------------------------------------------------------

log "ballast backup $SVC"
ballast backup "$SVC"

if [ -z "$(find "$REPOS" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
  echo "FAIL: repos directory is empty after backup" >&2
  exit 1
fi

# --- snapshots ---------------------------------------------------------------

log "ballast snapshots $SVC"
ballast snapshots "$SVC"

# --- restore -------------------------------------------------------------

log "ballast restore $SVC"
ballast restore "$SVC" --target /restore

RESTORED_FILE="$(find "$RESTORE" -name canary.txt | head -1)"
if [ -z "$RESTORED_FILE" ]; then
  echo "FAIL: canary.txt not found under $RESTORE after restore" >&2
  exit 1
fi
RESTORED_CANARY="$(cat "$RESTORED_FILE")"

echo "original canary:  $ORIGINAL_CANARY"
echo "restored canary:  $RESTORED_CANARY"
if [ "$ORIGINAL_CANARY" != "$RESTORED_CANARY" ]; then
  echo "FAIL: restored canary does not match the original" >&2
  exit 1
fi
echo "PASS: restored canary byte-matches the original"

# --- daemon smoke test ------------------------------------------------------

if [ "$SKIP_DAEMON" -eq 1 ]; then
  log "skipping daemon smoke test (--skip-daemon)"
  exit 0
fi

log "daemon smoke test: ballast daemon with BALLAST_SCHEDULE=@every 1m"
docker rm -f "$DAEMON" >/dev/null 2>&1 || true
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@every 1m" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

echo "waiting up to 90s for the daemon's first scheduled backup..."
for i in $(seq 1 45); do
  if docker logs "$DAEMON" 2>&1 | grep -q "Backup OK: $SVC"; then
    echo "daemon fired a scheduled backup"
    break
  fi
  sleep 2
done

docker logs "$DAEMON" 2>&1 | tail -20

SNAP_COUNT="$(ballast snapshots "$SVC" | tail -n +2 | wc -l)"
echo "snapshot count after daemon run: $SNAP_COUNT"
if [ "$SNAP_COUNT" -lt 2 ]; then
  echo "FAIL: expected at least 2 snapshots (one-shot + daemon), got $SNAP_COUNT" >&2
  exit 1
fi
echo "PASS: daemon produced a second snapshot"

log "done"
