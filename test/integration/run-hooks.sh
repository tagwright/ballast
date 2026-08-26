#!/usr/bin/env bash
# Ballast exec pre/post hook integration test: the third real end-to-end
# exercise of Ballast against a live Docker socket, proving
# internal/orchestrator's runHook path (RunBackup steps 3 and 9), which
# until this script existed had never actually been run by any test.
#
# It builds the image, starts a throwaway labeled container whose
# ballast.exec.pre and ballast.exec.post labels each write a distinct marker
# file into the same volume that gets backed up, runs a real `ballast
# backup`, and proves three things: (1) both hooks actually executed inside
# the container (docker exec reads each marker back), (2) the ordering the
# doc comment on orchestrator.RunBackup promises: restoring the snapshot
# shows the pre-marker present and the post-marker absent, because the
# filesystem backup step runs strictly between the two hooks, and (3) a
# pre-hook that exits non-zero aborts the run (no snapshot is written) while
# the post-hook still runs regardless, using a second throwaway container.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volume) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-hooks.sh [--keep]
#   --keep  skip cleanup at the end (for debugging a failure)

set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../.." && pwd)"

KEEP=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

SVC=ballast-itest-hooks-svc
VOL=ballast-itest-hooks-data
SVC_FAIL=ballast-itest-hooks-fail
IMAGE=ballast:itest
CFG="$HARNESS_DIR/hooks.itest.yml"
REPOS="$HARNESS_DIR/repos-hooks"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-hooks"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker rm -f "$SVC_FAIL" >/dev/null 2>&1 || true
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
  echo "NOTE: volumes root differs from the default baked into hooks.itest.yml (/var/lib/docker/volumes)." >&2
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

ballast() {
  docker run --rm \
    -e BALLAST_ENABLE_EXEC=true \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# --- test service with pre/post hooks ----------------------------------------

log "creating $SVC with exec.pre/exec.post marker hooks"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.exec.pre="date -u +%s > /data/pre-marker" \
  --label ballast.exec.post="date -u +%s > /data/post-marker" \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-hooks-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

sleep 1
ORIGINAL_CANARY="$(docker exec "$SVC" cat /data/canary.txt)"
echo "canary: $ORIGINAL_CANARY"

if docker exec "$SVC" test -e /data/pre-marker || docker exec "$SVC" test -e /data/post-marker; then
  echo "FAIL: a marker file already exists before backup ran" >&2
  exit 1
fi

# --- backup: both hooks must run ----------------------------------------------

log "ballast backup $SVC (exec pre/post hooks)"
ballast backup "$SVC"

log "confirming both hooks actually executed inside the container"
PRE_MARK="$(docker exec "$SVC" cat /data/pre-marker 2>/dev/null || true)"
if [ -z "$PRE_MARK" ]; then
  echo "FAIL: /data/pre-marker not found inside $SVC after backup; exec.pre did not run" >&2
  exit 1
fi
echo "PASS: exec.pre ran inside the container (marker: $PRE_MARK)"

POST_MARK="$(docker exec "$SVC" cat /data/post-marker 2>/dev/null || true)"
if [ -z "$POST_MARK" ]; then
  echo "FAIL: /data/post-marker not found inside $SVC after backup; exec.post did not run" >&2
  exit 1
fi
echo "PASS: exec.post ran inside the container (marker: $POST_MARK)"

# --- restore: prove ordering --------------------------------------------------
#
# The pre-marker is written before the filesystem backup step; the
# post-marker is written after it (RunBackup step 9, after step 5's fs
# backup and step 7's restart). So the snapshot itself should contain the
# pre-marker but NOT the post-marker: a direct, data-level proof of
# ordering rather than an inference from timestamps.

log "ballast restore $SVC"
ballast restore "$SVC" --target /restore

RESTORED_CANARY_FILE="$(find "$RESTORE" -name canary.txt | head -1)"
if [ -z "$RESTORED_CANARY_FILE" ]; then
  echo "FAIL: canary.txt not found under $RESTORE after restore" >&2
  exit 1
fi
RESTORED_CANARY="$(cat "$RESTORED_CANARY_FILE")"
if [ "$ORIGINAL_CANARY" != "$RESTORED_CANARY" ]; then
  echo "FAIL: restored canary does not match the original" >&2
  exit 1
fi

RESTORED_PRE="$(find "$RESTORE" -name pre-marker | head -1)"
RESTORED_POST="$(find "$RESTORE" -name post-marker | head -1)"
if [ -z "$RESTORED_PRE" ]; then
  echo "FAIL: pre-marker missing from the restored snapshot; exec.pre did not run before the backup" >&2
  exit 1
fi
if [ -n "$RESTORED_POST" ]; then
  echo "FAIL: post-marker unexpectedly present in the restored snapshot; exec.post ran before the backup instead of after" >&2
  exit 1
fi
echo "PASS: snapshot contains the pre-marker but not the post-marker: exec.pre ran before the backup, exec.post ran after"

# --- failure path: a non-zero exec.pre must abort the run, but exec.post ----
# --- must still run -----------------------------------------------------------

log "creating $SVC_FAIL with a failing exec.pre"
docker rm -f "$SVC_FAIL" >/dev/null 2>&1 || true
docker run -d --name "$SVC_FAIL" \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes=none \
  --label ballast.exec.pre="exit 1" \
  --label ballast.exec.post="touch /tmp/post-ran-proof" \
  busybox sh -c 'sleep 3600'
sleep 1

log "ballast backup $SVC_FAIL (expected to fail)"
if ballast backup "$SVC_FAIL"; then
  echo "FAIL: backup of $SVC_FAIL succeeded despite a non-zero exec.pre" >&2
  exit 1
fi
echo "PASS: backup aborted (non-zero exit) as expected"

log "confirming exec.post still ran despite the aborted backup"
if ! docker exec "$SVC_FAIL" test -f /tmp/post-ran-proof; then
  echo "FAIL: /tmp/post-ran-proof not found inside $SVC_FAIL; exec.post did not run after the aborted backup" >&2
  exit 1
fi
echo "PASS: exec.post ran even though exec.pre aborted the run"

log "ballast snapshots $SVC_FAIL"
FAIL_SNAP_OUT="$(ballast snapshots "$SVC_FAIL")"
echo "$FAIL_SNAP_OUT"
FAIL_SNAP_COUNT="$(echo "$FAIL_SNAP_OUT" | tail -n +2 | grep -c . || true)"
if [ "$FAIL_SNAP_COUNT" -ne 0 ]; then
  echo "FAIL: expected 0 snapshots for $SVC_FAIL after a failed exec.pre, got $FAIL_SNAP_COUNT" >&2
  exit 1
fi
echo "PASS: no snapshot was taken when exec.pre failed"

log "done"
