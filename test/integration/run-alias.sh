#!/usr/bin/env bash
# Ballast tagwright.backup.* alias integration test: proves the org-namespaced
# label prefix (internal/discovery/labels.go's tagwrightPrefix) discovers and
# backs up a service identically to ballast.*, end to end against a live
# Docker socket. Unit tests already cover the alias reaching individual
# BackupSpec fields (internal/discovery/notify_test.go's
# TestDiscoverNotifyLabelsSet mixes one tagwright.backup.* label in), but no
# itest before this one has ever driven a real backup/restore round trip
# using ONLY the tagwright.backup.* prefix, per docs/TESTING.md's coverage
# matrix.
#
# A single labeled container, entirely under tagwright.backup.* (enable,
# repo, name, tags, retention.last): a real "ballast backup", snapshot
# listing (confirming the tags label reached the snapshot), restore, and a
# byte-diff against the original canary.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container, volume) or tagged "ballast:itest" (image). It
# never touches any other container, volume, or image. Cleanup runs on exit
# (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-alias.sh [--keep]
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

SERVICE=ballast-itest-alias
SVC=ballast-itest-alias-svc
VOL=ballast-itest-alias-data
IMAGE=ballast:itest
CFG="$HARNESS_DIR/alias.itest.yml"
REPOS="$HARNESS_DIR/repos-alias"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-alias"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into alias.itest.yml (/var/lib/docker/volumes)." >&2
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

# --- test service, labeled entirely under tagwright.backup.* ----------------

log "creating $SVC with tagwright.backup.* labels only (no ballast.* labels at all)"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label tagwright.backup.enable=true \
  --label tagwright.backup.repo=local \
  --label tagwright.backup.name="$SERVICE" \
  --label tagwright.backup.tags=itest-alias \
  --label tagwright.backup.retention.last=5 \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-alias-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

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

log "ballast backup $SERVICE (discovered entirely via tagwright.backup.*)"
ballast backup "$SERVICE"

if [ -z "$(find "$REPOS" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
  echo "FAIL: repos directory is empty after backup" >&2
  exit 1
fi
echo "PASS: tagwright.backup.enable/repo/name discovered and backed up the service"

# --- snapshots: confirm tagwright.backup.tags reached the snapshot ----------

log "ballast snapshots $SERVICE"
SNAP_OUT="$(ballast snapshots "$SERVICE")"
echo "$SNAP_OUT"
if ! echo "$SNAP_OUT" | grep -q "itest-alias"; then
  echo "FAIL: snapshot tags do not include 'itest-alias'; tagwright.backup.tags was not applied" >&2
  exit 1
fi
echo "PASS: tagwright.backup.tags reached the snapshot's tags"

# --- restore -------------------------------------------------------------

log "ballast restore $SERVICE"
ballast restore "$SERVICE" --target /restore

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
echo "PASS: restored canary byte-matches the original, discovered and backed up entirely through tagwright.backup.*"

log "done"
