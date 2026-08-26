#!/usr/bin/env bash
# Ballast ballast.repo.path override integration test: proves a service's
# repository actually lands at the labeled sub-path under its destination
# (internal/orchestrator/backup.go's joinRepoURL), not at the default
# service-name path discovery.Discover falls back to when the label is
# absent.
#
# It creates one labeled service with ballast.repo.path=custom-repo-path,
# runs a real backup, and asserts directly on the host-visible repos
# directory: the overridden sub-path exists and holds a real initialized
# restic repository (a "config" object), and the default service-name path
# does NOT exist at all -- the positive and the negative, both needed to
# actually prove an override rather than merely "a repo exists somewhere".
# It then restores and byte-diffs the canary, proving the override is
# consistent at both write and read time (the same spec.RepoPath discovery
# resolves both times).
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container, volume) or tagged "ballast:itest" (image). It
# never touches any other container, volume, or image. Cleanup runs on exit
# (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-repo-path.sh [--keep]
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

SVC=ballast-itest-repopath-svc
VOL=ballast-itest-repopath-data
IMAGE=ballast:itest
CFG="$HARNESS_DIR/repo-path.itest.yml"
REPOS="$HARNESS_DIR/repos-repo-path"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-repo-path"
OVERRIDE_PATH="custom-repo-path"

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
  echo "NOTE: volumes root differs from the default baked into repo-path.itest.yml (/var/lib/docker/volumes)." >&2
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

# --- test service: ballast.repo.path=$OVERRIDE_PATH -------------------------

log "creating $SVC (ballast.repo.path=$OVERRIDE_PATH) with canary data in $VOL"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.repo.path="$OVERRIDE_PATH" \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-repopath-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

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

# --- the actual proof: repo lands at the OVERRIDDEN path, not the default ---

log "confirming the repository landed at the overridden sub-path, not the default service-name path"
if [ ! -f "$REPOS/$OVERRIDE_PATH/config" ]; then
  echo "FAIL: no initialized restic repository found at $REPOS/$OVERRIDE_PATH (ballast.repo.path did not take effect)" >&2
  echo "contents of $REPOS:" >&2
  find "$REPOS" -maxdepth 2 >&2 || true
  exit 1
fi
echo "PASS: repository config found at the overridden path $REPOS/$OVERRIDE_PATH"

if [ -e "$REPOS/$SVC" ]; then
  echo "FAIL: a repository also exists at the default service-name path $REPOS/$SVC; ballast.repo.path should have replaced it entirely, not added alongside it" >&2
  exit 1
fi
echo "PASS: no repository exists at the default (unoverridden) service-name path $REPOS/$SVC"

# --- restore, proving the override is consistent at read time too -----------

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
echo "PASS: restored canary byte-matches the original, read back through the same overridden repo.path"

log "done"
