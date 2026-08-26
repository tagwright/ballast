#!/usr/bin/env bash
# Ballast ballast.*/tagwright.backup.* prefix-conflict integration test:
# proves internal/discovery/labels.go's normalizeLabels conflict rule (the
# same label suffix set to two different values under the two recognized
# prefixes) is actually rejected and surfaced end to end, against both call
# paths that read discovery.Discover's result:
#
#   1. The daemon's discoverOne, which only checks the error and never needs
#      the spec -- this path always worked correctly, and this script proves
#      it live: "daemon: discovery failed" in the logs, no backup ever fires
#      for the conflicted service, and the daemon keeps running.
#   2. "ballast backup <service>", which matches a container by
#      BackupSpec.Service *before* checking the error. Before this pass's
#      fix (see the commit that added the mergeExcludes/prefix-conflict spec
#      fix to internal/discovery/discovery.go), normalizeLabels' conflict
#      error made Discover return a nil spec, so this path's "if spec == nil
#      { continue }" silently skipped the very container being looked up and
#      reported a misleading "service ... not found" instead of the real
#      conflict -- the same class of bug 8770193 already fixed for
#      validate()'s errors, just at a different point inside Discover. This
#      script is the live regression check for that fix.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container) or tagged "ballast:itest" (image). It never
# touches any other container, volume, or image. Cleanup runs on exit
# (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-conflict.sh [--keep]
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

SVC=ballast-itest-conflict-svc
DAEMON=ballast-itest-conflict-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/conflict.itest.yml"
REPOS="$HARNESS_DIR/repos-conflict"
SECRETS="$HARNESS_DIR/secrets"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$DAEMON" "$SVC" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into conflict.itest.yml (/var/lib/docker/volumes)." >&2
  echo "      Add a host_roots entry mapping $VOLUMES_ROOT to itself before running this harness." >&2
  exit 1
fi

mkdir -p "$REPOS" "$SECRETS"

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
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

docker rm -f "$DAEMON" "$SVC" >/dev/null 2>&1 || true

log "creating $SVC with a real ballast.*/tagwright.backup.* conflict (ballast.repo=A vs tagwright.backup.repo=B)"
docker run -d --name "$SVC" \
  --label ballast.enable=true \
  --label ballast.repo=A \
  --label tagwright.backup.repo=B \
  busybox sh -c 'sleep 3600'
sleep 1

# =============================================================================
# 1. Daemon path: the conflict must be logged and alerted, never backed up.
# =============================================================================

log "starting $DAEMON (BALLAST_SCHEDULE=@every 8s) pointed at the conflicted container"
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@every 8s" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

log "waiting up to 20s for the daemon to log the discovery conflict"
LOGGED=0
for i in $(seq 1 10); do
  if docker logs "$DAEMON" 2>&1 | grep -q "daemon: discovery failed"; then
    LOGGED=1
    break
  fi
  sleep 2
done
if [ "$LOGGED" -ne 1 ]; then
  echo "FAIL: daemon never logged 'daemon: discovery failed' for $SVC within 20s" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
CONFLICT_LINE="$(docker logs "$DAEMON" 2>&1 | grep "daemon: discovery failed")"
echo "$CONFLICT_LINE"
if ! echo "$CONFLICT_LINE" | grep -q "conflicts with"; then
  echo "FAIL: discovery failure line does not mention 'conflicts with'" >&2
  exit 1
fi
echo "PASS: the daemon surfaced the prefix conflict via discoverOne"

log "waiting 20s (more than two schedule intervals) to confirm no backup ever fires for $SVC"
sleep 20
if docker logs "$DAEMON" 2>&1 | grep -q "Backup OK: $SVC"; then
  echo "FAIL: a backup ran for the conflicted service $SVC; discovery should have rejected it every time" >&2
  exit 1
fi
if ! docker inspect -f '{{.State.Running}}' "$DAEMON" | grep -q true; then
  echo "FAIL: $DAEMON is not running after repeatedly re-discovering the conflicted container" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: no backup ever fired for the conflicted service, and the daemon kept running"

# =============================================================================
# 2. CLI path: "ballast backup <service>" must surface the real conflict,
#    not a misleading "not found" (the regression this pass's fix closed).
# =============================================================================

log "ballast backup $SVC (expected to fail with the real conflict, not 'not found')"
set +e
BACKUP_OUT="$(ballast backup "$SVC" 2>&1)"
BACKUP_STATUS=$?
set -e
echo "$BACKUP_OUT"
if [ "$BACKUP_STATUS" -eq 0 ]; then
  echo "FAIL: ballast backup succeeded for a container with a ballast.*/tagwright.backup.* conflict" >&2
  exit 1
fi
if echo "$BACKUP_OUT" | grep -qi "not found"; then
  echo "FAIL: ballast backup reported a misleading 'not found' instead of the real conflict" >&2
  exit 1
fi
if ! echo "$BACKUP_OUT" | grep -q "conflicts with"; then
  echo "FAIL: ballast backup's error does not mention the real label conflict ('conflicts with')" >&2
  exit 1
fi
echo "PASS: ballast backup surfaced the real prefix conflict, not a misleading 'not found'"

log "done"
