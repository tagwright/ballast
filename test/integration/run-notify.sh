#!/usr/bin/env bash
# Ballast live notification delivery integration test: proves the real gap
# no other itest closes: config -> internal/daemon.BuildNotifier ->
# orchestrator.reportOutcome -> beacon -> an actual notification arriving at
# a real channel, driven end to end through Ballast's own orchestrator (not
# beacon exercised in isolation, which is beacon's own itest suite's job).
#
# It builds the image, starts a throwaway binwiederhier/ntfy server on a
# throwaway network, points notify.itest.yml's one notifications channel at
# it, and runs three real `ballast backup`s:
#
#   1. a plain successful backup: confirms a message actually lands in the
#      ntfy topic, with the right title, body, and default (info) priority.
#   2. a service labeled ballast.notify.suppress=true: confirms NO message
#      lands for it, proving the per-service suppress control actually
#      mutes the Notify call.
#   3. a service labeled ballast.notify.on-success=true: confirms a message
#      lands at ntfy's "high" (4) priority instead of "default" (3), proving
#      the per-service on-success escalation actually raises the level.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volumes, network) or tagged "ballast:itest"
# (image). It never touches any other container, volume, network, or image.
# Cleanup runs on exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-notify.sh [--keep]
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

NET=ballast-itest-notify-net
NTFY=ballast-itest-ntfy
TOPIC=ballast-itest
SVC_OK=ballast-itest-notify-ok
VOL_OK=ballast-itest-notify-ok-data
SVC_SUP=ballast-itest-notify-suppress
VOL_SUP=ballast-itest-notify-suppress-data
SVC_WARN=ballast-itest-notify-warn
VOL_WARN=ballast-itest-notify-warn-data
IMAGE=ballast:itest
CFG="$HARNESS_DIR/notify.itest.yml"
REPOS="$HARNESS_DIR/repos-notify"
SECRETS="$HARNESS_DIR/secrets"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC_OK" "$SVC_SUP" "$SVC_WARN" >/dev/null 2>&1 || true
  docker rm -f "$NTFY" >/dev/null 2>&1 || true
  docker volume rm "$VOL_OK" "$VOL_SUP" "$VOL_WARN" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
echo "DockerRootDir: $DOCKER_ROOT"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into notify.itest.yml (/var/lib/docker/volumes)." >&2
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

# --- network + ntfy -----------------------------------------------------------

log "creating $NET"
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null

log "starting $NTFY (binwiederhier/ntfy)"
docker rm -f "$NTFY" >/dev/null 2>&1 || true
docker run -d --name "$NTFY" --network "$NET" \
  binwiederhier/ntfy serve >/dev/null

log "waiting for $NTFY to answer"
docker run --rm --network "$NET" curlimages/curl:latest \
  -sf --retry 30 --retry-delay 1 --retry-connrefused --retry-all-errors \
  "http://$NTFY:80/v1/health" >/dev/null
echo "$NTFY is ready"

ballast() {
  docker run --rm --network "$NET" \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# ntfy_poll fetches every currently cached message on $TOPIC as JSON lines.
ntfy_poll() {
  docker run --rm --network "$NET" curlimages/curl:latest \
    -s "http://$NTFY:80/$TOPIC/json?poll=1"
}

# find_ntfy_message prints the JSON object for the newest cached message
# whose "title" equals $1, and exits 0. Exits 1 (printing nothing) if no
# such message is in the topic yet.
find_ntfy_message() {
  ntfy_poll | python3 -c '
import json, sys
title = sys.argv[1]
found = None
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        m = json.loads(line)
    except ValueError:
        continue
    if m.get("title") == title:
        found = m
if found is None:
    sys.exit(1)
print(json.dumps(found))
' "$1"
}

json_field() {
  python3 -c 'import json, sys; print(json.loads(sys.argv[1]).get(sys.argv[2], ""))' "$1" "$2"
}

# --- 1. plain successful backup: a message must actually arrive -------------

log "creating $SVC_OK"
docker rm -f "$SVC_OK" >/dev/null 2>&1 || true
docker volume rm "$VOL_OK" >/dev/null 2>&1 || true
docker run -d --name "$SVC_OK" \
  -v "$VOL_OK":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  busybox sh -c 'mkdir -p /data && echo canary > /data/canary.txt && sleep 3600'
sleep 1

log "ballast backup $SVC_OK"
ballast backup "$SVC_OK"

log "polling ntfy for the $SVC_OK notification"
TITLE_OK="Backup OK: $SVC_OK"
if ! MSG_OK="$(find_ntfy_message "$TITLE_OK")"; then
  echo "FAIL: no ntfy message titled '$TITLE_OK' arrived after a successful backup" >&2
  exit 1
fi
echo "$MSG_OK"
BODY_OK="$(json_field "$MSG_OK" message)"
PRIORITY_OK="$(json_field "$MSG_OK" priority)"
echo "body: $BODY_OK / priority: $PRIORITY_OK"
if [[ "$BODY_OK" != *"Backup completed for $SVC_OK"* ]]; then
  echo "FAIL: notification body does not describe the backup outcome: $BODY_OK" >&2
  exit 1
fi
if [ "$PRIORITY_OK" != "3" ]; then
  echo "FAIL: expected ntfy priority 3 (default, LevelInfo) for a plain success, got $PRIORITY_OK" >&2
  exit 1
fi
echo "PASS: a real notification describing the successful backup arrived at ntfy, at default priority"

# --- 2. ballast.notify.suppress=true: no message must arrive ----------------

log "creating $SVC_SUP with ballast.notify.suppress=true"
docker rm -f "$SVC_SUP" >/dev/null 2>&1 || true
docker volume rm "$VOL_SUP" >/dev/null 2>&1 || true
docker run -d --name "$SVC_SUP" \
  -v "$VOL_SUP":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.notify.suppress=true \
  busybox sh -c 'mkdir -p /data && echo canary > /data/canary.txt && sleep 3600'
sleep 1

log "ballast backup $SVC_SUP"
ballast backup "$SVC_SUP"

log "polling ntfy to confirm no notification arrived for $SVC_SUP"
TITLE_SUP="Backup OK: $SVC_SUP"
if MSG_SUP="$(find_ntfy_message "$TITLE_SUP")"; then
  echo "FAIL: a notification arrived for $SVC_SUP despite ballast.notify.suppress=true: $MSG_SUP" >&2
  exit 1
fi
echo "PASS: no notification arrived for the suppressed service"

# --- 3. ballast.notify.on-success=true: a Warning-level message must arrive -

log "creating $SVC_WARN with ballast.notify.on-success=true"
docker rm -f "$SVC_WARN" >/dev/null 2>&1 || true
docker volume rm "$VOL_WARN" >/dev/null 2>&1 || true
docker run -d --name "$SVC_WARN" \
  -v "$VOL_WARN":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.notify.on-success=true \
  busybox sh -c 'mkdir -p /data && echo canary > /data/canary.txt && sleep 3600'
sleep 1

log "ballast backup $SVC_WARN"
ballast backup "$SVC_WARN"

log "polling ntfy for the $SVC_WARN notification"
TITLE_WARN="Backup OK: $SVC_WARN"
if ! MSG_WARN="$(find_ntfy_message "$TITLE_WARN")"; then
  echo "FAIL: no ntfy message titled '$TITLE_WARN' arrived after a successful backup with notify.on-success=true" >&2
  exit 1
fi
echo "$MSG_WARN"
PRIORITY_WARN="$(json_field "$MSG_WARN" priority)"
echo "priority: $PRIORITY_WARN"
if [ "$PRIORITY_WARN" != "4" ]; then
  echo "FAIL: expected ntfy priority 4 (high, LevelWarning) for notify.on-success=true, got $PRIORITY_WARN" >&2
  exit 1
fi
echo "PASS: notify.on-success=true escalated a successful backup's notification to Warning-level (ntfy priority 4)"

log "done"
