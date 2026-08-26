#!/usr/bin/env bash
# Ballast duplicate-service-name integration test: proves
# internal/daemon/registry.go's register() duplicate-rejection rule against
# a live daemon, not just the unit tests added alongside it in 4643aca. Two
# containers resolving to the SAME service name (via ballast.name) must
# result in exactly one of them being backed up, ever, with the conflict
# surfaced in the daemon's logs -- not a silent double-backup (which would
# corrupt the shared repository as two unrelated containers race to write
# snapshots under the same host/repo identity) and not a daemon crash.
#
# The first container to register (created and discovered before the second
# even exists) wins, per register()'s "first container to claim a service
# name keeps it" rule; the second is rejected. This script proves the winner
# stays the winner across multiple scheduled rounds, not just the first one:
# after the daemon logs the rejection, it waits through another full
# schedule interval and restores the *latest* snapshot, confirming the
# canary content still matches the first container's data and never the
# second's.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volumes) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-dupe.sh [--keep]
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

SERVICE=ballast-itest-dupe
SVC1=ballast-itest-dupe-1
SVC2=ballast-itest-dupe-2
VOL1=ballast-itest-dupe-vol-1
VOL2=ballast-itest-dupe-vol-2
DAEMON=ballast-itest-dupe-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/dupe.itest.yml"
REPOS="$HARNESS_DIR/repos-dupe"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-dupe"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$DAEMON" "$SVC1" "$SVC2" >/dev/null 2>&1 || true
  docker volume rm "$VOL1" "$VOL2" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into dupe.itest.yml (/var/lib/docker/volumes)." >&2
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
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

docker rm -f "$DAEMON" "$SVC1" "$SVC2" >/dev/null 2>&1 || true
docker volume rm "$VOL1" "$VOL2" >/dev/null 2>&1 || true

# --- first container claims the service name ---------------------------------

log "creating $SVC1 (ballast.name=$SERVICE), the first to exist"
docker run -d --name "$SVC1" \
  -v "$VOL1":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.name="$SERVICE" \
  busybox sh -c 'mkdir -p /data && echo "canary-from-svc1-$(date +%s)" > /data/canary.txt && sleep 3600'
sleep 1
CANARY_1="$(docker exec "$SVC1" cat /data/canary.txt)"
echo "svc1 canary: $CANARY_1"

log "starting $DAEMON (BALLAST_SCHEDULE=@every 20s) with only $SVC1 present"
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@every 20s" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

log "waiting up to 60s for the daemon's initial discovery to back up $SVC1"
FIRST_OK=0
for i in $(seq 1 30); do
  if docker logs "$DAEMON" 2>&1 | grep -q "Backup OK: $SERVICE"; then
    FIRST_OK=1
    break
  fi
  sleep 2
done
if [ "$FIRST_OK" -ne 1 ]; then
  echo "FAIL: daemon never logged a successful backup for $SERVICE within 60s" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: $SVC1 registered as $SERVICE and was backed up"

# --- second container claims the SAME service name ---------------------------

log "creating $SVC2 (also ballast.name=$SERVICE), a different container"
docker run -d --name "$SVC2" \
  -v "$VOL2":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.name="$SERVICE" \
  busybox sh -c 'mkdir -p /data && echo "canary-from-svc2-$(date +%s)" > /data/canary.txt && sleep 3600'
sleep 1
CANARY_2="$(docker exec "$SVC2" cat /data/canary.txt)"
echo "svc2 canary: $CANARY_2"

log "waiting up to 20s for the daemon to reject $SVC2 as a duplicate"
REJECTED=0
for i in $(seq 1 10); do
  if docker logs "$DAEMON" 2>&1 | grep -q "duplicate service name, skipping"; then
    REJECTED=1
    break
  fi
  sleep 2
done
if [ "$REJECTED" -ne 1 ]; then
  echo "FAIL: daemon never logged a duplicate-service-name rejection for $SVC2 within 20s" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
REJECT_LINE="$(docker logs "$DAEMON" 2>&1 | grep "duplicate service name, skipping")"
echo "$REJECT_LINE"
if ! echo "$REJECT_LINE" | grep -q "service=$SERVICE"; then
  echo "FAIL: rejection log line does not name service=$SERVICE" >&2
  exit 1
fi
echo "PASS: the daemon rejected $SVC2 as a duplicate of $SERVICE and surfaced it in the logs"

# --- the daemon must not have crashed, and must keep backing up SVC1 only ----

if ! docker inspect -f '{{.State.Running}}' "$DAEMON" | grep -q true; then
  echo "FAIL: $DAEMON is not running after the duplicate registration attempt" >&2
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: the daemon is still running after the duplicate registration attempt (no crash)"

log "waiting 25s (one more schedule interval) to confirm the winner keeps winning"
sleep 25

ballast restore "$SERVICE" --target /restore/latest
LATEST_FILE="$(find "$RESTORE/latest" -name canary.txt | head -1)"
if [ -z "$LATEST_FILE" ]; then
  echo "FAIL: canary.txt not found in the latest restored snapshot for $SERVICE" >&2
  exit 1
fi
LATEST_CANARY="$(cat "$LATEST_FILE")"
echo "latest restored canary: $LATEST_CANARY"
if [ "$LATEST_CANARY" != "$CANARY_1" ]; then
  echo "FAIL: latest snapshot's canary ($LATEST_CANARY) does not match $SVC1's ($CANARY_1); a rejected duplicate somehow got backed up" >&2
  exit 1
fi
echo "PASS: even after another schedule round, only $SVC1's data was ever backed up under $SERVICE"

log "done"
