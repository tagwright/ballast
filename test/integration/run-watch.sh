#!/usr/bin/env bash
# Ballast live daemon watch integration test: the first real end-to-end
# exercise of internal/daemon/watch.go's socket-event path. Every prior
# itest run only proved startup discovery (discoverAll, run once before the
# watch loop even starts); this one proves the watch loop itself.
#
# It starts a real `ballast daemon` (fast schedule) pointed at a service
# that does NOT exist yet, then `docker run`s a new labeled container WHILE
# the daemon is running and confirms three things from the daemon's own
# behavior, not just its logs: (1) the daemon discovers the new container
# via a real Docker "start" event and registers a scheduled job for it
# (proven by a snapshot actually landing in the repository), (2) removing
# that container (`docker rm -f`, a real "die"+"destroy" event pair) drops
# its scheduled job, proven by "daemon: service unregistered" in the logs,
# and (3) the drop is real, not just logged: after removal, waiting through
# at least one more schedule interval produces no further snapshot.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volume) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-watch.sh [--keep]
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

SVC=ballast-itest-watch-svc
VOL=ballast-itest-watch-data
DAEMON=ballast-itest-watch-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/watch.itest.yml"
REPOS="$HARNESS_DIR/repos-watch"
SECRETS="$HARNESS_DIR/secrets"

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
  rm -rf "${REPOS:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
echo "DockerRootDir: $DOCKER_ROOT"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into watch.itest.yml (/var/lib/docker/volumes)." >&2
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

# --- daemon, started before $SVC exists at all -------------------------------

docker rm -f "$DAEMON" "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true

log "starting $DAEMON (BALLAST_SCHEDULE=@every 25s) with no $SVC container yet"
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@every 25s" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

# Give the daemon time to finish its initial discovery pass and start
# subscribing to the Docker event stream before $SVC is created, so the
# discovery this test proves really does come from the watch loop's "start"
# event handling, not a race with startup discovery.
sleep 3
if ! docker inspect -f '{{.State.Running}}' "$DAEMON" | grep -q true; then
  echo "FAIL: $DAEMON is not running after startup" >&2
  docker logs "$DAEMON" 2>&1 | tail -30
  exit 1
fi

# --- create the service the daemon has never seen ----------------------------

log "creating $SVC (the daemon starts with no knowledge of it)"
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-watch-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

# --- prove discovery: the daemon's watch loop must pick it up and back it up -

log "waiting up to 90s for the daemon to discover $SVC via a start event and back it up"
DISCOVERED=0
for i in $(seq 1 45); do
  if docker logs "$DAEMON" 2>&1 | grep -q "Backup OK: $SVC"; then
    DISCOVERED=1
    break
  fi
  sleep 2
done
if [ "$DISCOVERED" -ne 1 ]; then
  echo "FAIL: daemon never logged a successful backup for $SVC within 60s" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: daemon discovered $SVC via the watch loop and backed it up"

SNAP_COUNT_BEFORE="$(ballast snapshots "$SVC" --destination local --repo-path "$SVC" | tail -n +2 | grep -c . || true)"
echo "snapshot count for $SVC before removal: $SNAP_COUNT_BEFORE"
if [ "$SNAP_COUNT_BEFORE" -lt 1 ]; then
  echo "FAIL: expected at least 1 snapshot for $SVC, got $SNAP_COUNT_BEFORE" >&2
  exit 1
fi
echo "PASS: a real snapshot exists for the container discovered while the daemon was running"

# --- remove the container and prove the daemon drops the job -----------------

log "removing $SVC (docker rm -f: a real die+destroy event pair)"
docker rm -f "$SVC" >/dev/null

log "waiting up to 20s for the daemon to log the unregistration"
UNREGISTERED=0
for i in $(seq 1 10); do
  if docker logs "$DAEMON" 2>&1 | grep -q "daemon: service unregistered" && \
     docker logs "$DAEMON" 2>&1 | grep "daemon: service unregistered" | grep -q "service=$SVC"; then
    UNREGISTERED=1
    break
  fi
  sleep 2
done
if [ "$UNREGISTERED" -ne 1 ]; then
  echo "FAIL: daemon never logged 'daemon: service unregistered' for $SVC within 20s" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: daemon logged the removal of $SVC's scheduled job"

# --- prove the drop is real: no further backups fire -------------------------
#
# BACKUP_OK_AT_UNREGISTER is captured right after the unregistration is
# confirmed in the logs (not right after "docker rm -f", which could still
# race a backup the scheduler had already dispatched a moment earlier: per
# schedule.Scheduler.Remove's own doc comment, "a run already in flight...
# is not interrupted; it simply will not be rescheduled" -- a run dispatched
# a moment before the die event is expected behavior, not a bug, and would
# make this check flaky if measured from too early a point). The 25s
# schedule interval gives ample headroom between the first backup completing
# (detected above, typically within a couple of seconds) and $SVC's removal,
# so no such in-flight run should occur in the first place; this count is
# the belt-and-suspenders confirmation.

BACKUP_OK_AT_UNREGISTER="$(docker logs "$DAEMON" 2>&1 | grep -c "Backup OK: $SVC" || true)"
echo "\"Backup OK: $SVC\" count once unregistration is confirmed: $BACKUP_OK_AT_UNREGISTER"

log "waiting 30s (one full schedule interval plus slack) to confirm no further backup fires"
sleep 30
BACKUP_OK_AFTER="$(docker logs "$DAEMON" 2>&1 | grep -c "Backup OK: $SVC" || true)"
echo "\"Backup OK: $SVC\" count after the wait: $BACKUP_OK_AFTER"
if [ "$BACKUP_OK_AFTER" -ne "$BACKUP_OK_AT_UNREGISTER" ]; then
  echo "FAIL: a backup for $SVC fired after it was removed and unregistered ($BACKUP_OK_AT_UNREGISTER -> $BACKUP_OK_AFTER); the scheduled job was not actually dropped" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi

SNAP_COUNT_AFTER="$(ballast snapshots "$SVC" --destination local --repo-path "$SVC" | tail -n +2 | grep -c . || true)"
echo "snapshot count for $SVC after removal + wait: $SNAP_COUNT_AFTER"
if [ "$SNAP_COUNT_AFTER" -ne "$SNAP_COUNT_BEFORE" ]; then
  echo "FAIL: snapshot count changed after removal ($SNAP_COUNT_BEFORE -> $SNAP_COUNT_AFTER); the scheduled job was not actually dropped" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: no further backup fired after $SVC was removed; the daemon truly dropped the job"

log "done"
