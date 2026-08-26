#!/usr/bin/env bash
# Ballast stop-for-consistency integration test: proves the ballast.stop /
# BALLAST_ENABLE_STOP path in internal/orchestrator/backup.go's
# runBackupSteps, which until this script existed had never actually
# stopped and restarted a real container under test.
#
# It builds the image, starts a throwaway labeled container with
# ballast.stop=true, watches `docker events` for that container across a
# real `ballast backup`, and confirms: the container was actually stopped
# (a "die" event) and started again (a "start" event) in that order during
# the run, the container is running again with a fresh State.StartedAt once
# the command returns, and a real snapshot was written and restores
# correctly. It then confirms discovery's grammar rejects stop+stream as an
# incompatible combination on a second throwaway container.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volume) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-stop.sh [--keep]
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

SVC=ballast-itest-stop-svc
VOL=ballast-itest-stop-data
SVC_BAD=ballast-itest-stop-badcombo
IMAGE=ballast:itest
CFG="$HARNESS_DIR/stop.itest.yml"
REPOS="$HARNESS_DIR/repos-stop"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-stop"
EVENTS_LOG=""
EVENTS_PID=""

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ -n "$EVENTS_PID" ]; then
    kill "$EVENTS_PID" >/dev/null 2>&1 || true
    wait "$EVENTS_PID" 2>/dev/null || true
  fi
  if [ -n "$EVENTS_LOG" ]; then
    rm -f "$EVENTS_LOG" 2>/dev/null || true
  fi
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker rm -f "$SVC_BAD" >/dev/null 2>&1 || true
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
  echo "NOTE: volumes root differs from the default baked into stop.itest.yml (/var/lib/docker/volumes)." >&2
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
    -e BALLAST_ENABLE_STOP=true \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# --- test service with ballast.stop=true --------------------------------------

log "creating $SVC with ballast.stop=true"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
# Two things matter for this container's PID 1, or Runtime.Stop's SIGTERM
# would go unanswered and every stop would eat the full
# defaultStopTimeoutSeconds before a SIGKILL:
#   - "exec sleep 3600" (rather than leaving a bare "sleep 3600" as the
#     shell's last simple command) replaces the shell with sleep, so the
#     signal isn't sent to a shell that never forwards it to its child.
#   - "--init" runs sleep under tini rather than directly as PID 1: Linux
#     does not apply a signal's default disposition to PID 1 of a namespace
#     unless the process explicitly installs a handler for it (see pid_
#     namespaces(7)), and busybox's sleep installs none, so without --init
#     SIGTERM is silently ignored even once sleep IS PID 1.
docker run -d --init --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.stop=true \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-stop-canary-$(date +%s)" > /data/canary.txt && exec sleep 3600'

sleep 1
ORIGINAL_CANARY="$(docker exec "$SVC" cat /data/canary.txt)"
echo "canary: $ORIGINAL_CANARY"

RUNNING_BEFORE="$(docker inspect -f '{{.State.Running}}' "$SVC")"
STARTED_BEFORE="$(docker inspect -f '{{.State.StartedAt}}' "$SVC")"
echo "before backup: Running=$RUNNING_BEFORE StartedAt=$STARTED_BEFORE"
if [ "$RUNNING_BEFORE" != "true" ]; then
  echo "FAIL: $SVC is not running before the backup even started" >&2
  exit 1
fi

# --- watch docker events across the backup -----------------------------------

EVENTS_LOG="$(mktemp)"
log "watching docker events for $SVC (die, start) across the backup"
docker events --filter "container=$SVC" --filter "event=die" --filter "event=start" \
  --format '{{.Action}}' > "$EVENTS_LOG" 2>&1 &
EVENTS_PID=$!
sleep 1 # let the event listener attach before the backup runs

log "ballast backup $SVC (stop-for-consistency)"
ballast backup "$SVC"

sleep 2 # let any trailing event reach the log
kill "$EVENTS_PID" >/dev/null 2>&1 || true
wait "$EVENTS_PID" 2>/dev/null || true
EVENTS_PID=""

echo "--- docker events observed for $SVC ---"
cat "$EVENTS_LOG"
echo "----------------------------------------"

DIE_LINE="$(grep -n '^die$' "$EVENTS_LOG" | head -1 | cut -d: -f1 || true)"
START_LINE="$(grep -n '^start$' "$EVENTS_LOG" | head -1 | cut -d: -f1 || true)"
if [ -z "$DIE_LINE" ]; then
  echo "FAIL: no 'die' event observed for $SVC during the backup; the container may never have been stopped" >&2
  exit 1
fi
if [ -z "$START_LINE" ]; then
  echo "FAIL: no 'start' event observed for $SVC after the backup; the container may never have been restarted" >&2
  exit 1
fi
if [ "$DIE_LINE" -ge "$START_LINE" ]; then
  echo "FAIL: 'start' event ($START_LINE) did not come strictly after 'die' ($DIE_LINE)" >&2
  exit 1
fi
echo "PASS: $SVC was stopped (die) then started again (start), in that order, during the backup"

RUNNING_AFTER="$(docker inspect -f '{{.State.Running}}' "$SVC")"
STARTED_AFTER="$(docker inspect -f '{{.State.StartedAt}}' "$SVC")"
echo "after backup: Running=$RUNNING_AFTER StartedAt=$STARTED_AFTER"
if [ "$RUNNING_AFTER" != "true" ]; then
  echo "FAIL: $SVC is not running after the backup completed" >&2
  exit 1
fi
if [ "$STARTED_AFTER" = "$STARTED_BEFORE" ]; then
  echo "FAIL: $SVC's StartedAt did not change; it may not actually have been restarted" >&2
  exit 1
fi
echo "PASS: $SVC is running again with a fresh StartedAt"

# --- confirm the snapshot itself, taken while stopped, is correct -----------

log "ballast snapshots $SVC"
SNAP_OUT="$(ballast snapshots "$SVC")"
echo "$SNAP_OUT"
if [ "$(echo "$SNAP_OUT" | tail -n +2 | grep -c .)" -eq 0 ]; then
  echo "FAIL: no snapshot found for $SVC after a stop-for-consistency backup" >&2
  exit 1
fi

log "ballast restore $SVC"
ballast restore "$SVC" --target /restore
RESTORED_FILE="$(find "$RESTORE" -name canary.txt | head -1)"
if [ -z "$RESTORED_FILE" ]; then
  echo "FAIL: canary.txt not found under $RESTORE after restore" >&2
  exit 1
fi
RESTORED_CANARY="$(cat "$RESTORED_FILE")"
if [ "$ORIGINAL_CANARY" != "$RESTORED_CANARY" ]; then
  echo "FAIL: restored canary does not match the original" >&2
  exit 1
fi
echo "PASS: the snapshot taken while $SVC was stopped restores byte-for-byte"

# --- discovery grammar: stop=true is incompatible with stream backups -------

log "creating $SVC_BAD with both ballast.stop=true and a stream label"
docker rm -f "$SVC_BAD" >/dev/null 2>&1 || true
docker run -d --name "$SVC_BAD" \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes=none \
  --label ballast.stop=true \
  --label ballast.stream.x.command=true \
  busybox sh -c 'sleep 3600'
sleep 1

log "ballast backup $SVC_BAD (expected to be rejected at discovery)"
set +e
BAD_OUT="$(ballast backup "$SVC_BAD" 2>&1)"
BAD_STATUS=$?
set -e
echo "$BAD_OUT"
if [ "$BAD_STATUS" -eq 0 ]; then
  echo "FAIL: backup of $SVC_BAD succeeded despite stop=true + a stream label" >&2
  exit 1
fi
if ! echo "$BAD_OUT" | grep -qi "incompatible"; then
  echo "FAIL: expected an 'incompatible' validation error for stop=true + stream, got:" >&2
  echo "$BAD_OUT" >&2
  exit 1
fi
echo "PASS: discovery rejected stop=true combined with a stream backup as incompatible"

log "done"
