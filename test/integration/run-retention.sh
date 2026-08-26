#!/usr/bin/env bash
# Ballast retention/forget/prune/check integration test: proves the
# lifecycle paths run.sh, run-stream.sh, and run-s3.sh never exercised past
# "the call didn't error" — that a retention policy actually keeps and
# forgets the RIGHT snapshots, and that prune and check run clean against a
# real repository afterward.
#
# It builds the image, creates one throwaway labeled container with
# ballast.retention.last=3, forces five separate "ballast backup" runs
# (each one, per the real backup lifecycle, also runs Forget with that
# policy — see internal/orchestrator/backup.go's runBackupSteps), and
# records the snapshot ID each run produces. Because keep-last=3 runs after
# every single backup (not just once at the end), the surviving set after
# five runs must be exactly the last three snapshot IDs recorded, in order:
# this is asserted directly, not just a bare count.
#
# It then starts a real daemon with BALLAST_PRUNE_SCHEDULE and
# BALLAST_CHECK_SCHEDULE set to short intervals (its own backup schedule is
# pinned to @yearly so it can't fire and change the snapshot set mid-test),
# and confirms from the daemon's own logs that both
# internal/daemon/maintenance.go actions completed without error. The
# daemon is stopped before a final `restic check --read-data` is run
# directly against the repository (via the same image's bundled restic
# binary, using the password `ballast key` derives), covering the
# --read-data path the daemon's own CheckSchedule never exercises
# (maintenance.go hardcodes readData=false).
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container, volume) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-retention.sh [--keep]
#   --keep  skip cleanup at the end (for debugging a failure)
#
# What this does NOT prove: time-based retention (keep-daily/keep-weekly/
# etc.). Asserting those deterministically would require controlling
# restic's snapshot timestamps (e.g. faking the clock or spacing real runs
# across real days), which this harness does not attempt. See
# docs/TESTING.md for the honest accounting.

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

SVC=ballast-itest-retention-svc
VOL=ballast-itest-retention-data
DAEMON=ballast-itest-retention-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/retention.itest.yml"
REPOS="$HARNESS_DIR/repos-retention"
SECRETS="$HARNESS_DIR/secrets"
PWFILE="$HARNESS_DIR/retention-pw.tmp"

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
  rm -f "$PWFILE" 2>/dev/null || true
  rm -rf "${REPOS:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
echo "DockerRootDir: $DOCKER_ROOT"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into retention.itest.yml (/var/lib/docker/volumes)." >&2
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

# --- test service: keep-last=3 via ballast.retention.last -------------------

log "creating $SVC (ballast.retention.last=3) with canary data in $VOL"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.retention.last=3 \
  busybox sh -c 'mkdir -p /data && echo seed > /data/canary.txt && sleep 3600'
sleep 1

ballast() {
  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# latest_snapshot_id returns the most recently created snapshot ID for $SVC
# (the last row of "ballast snapshots", which is sorted by time ascending).
latest_snapshot_id() {
  ballast snapshots "$SVC" | tail -n +2 | tail -n 1 | awk '{print $2}'
}

# all_snapshot_ids returns every current snapshot ID for $SVC, one per line.
all_snapshot_ids() {
  ballast snapshots "$SVC" | tail -n +2 | awk '{print $2}'
}

# --- five backups, each one also applying keep-last=3 via Forget ------------

declare -a RECORDED
for i in 1 2 3 4 5; do
  log "backup $i/5"
  docker exec "$SVC" sh -c "echo iteration-$i-\$(date +%s%N) > /data/canary.txt"
  ballast backup "$SVC"
  id="$(latest_snapshot_id)"
  echo "snapshot recorded for iteration $i: $id"
  RECORDED[$i]="$id"
done

# --- assert the RIGHT snapshots survive: exactly the last 3 recorded --------

log "asserting keep-last=3 kept the newest 3 of 5 snapshots"
FINAL_IDS="$(all_snapshot_ids | sort)"
EXPECTED_IDS="$(printf '%s\n%s\n%s\n' "${RECORDED[3]}" "${RECORDED[4]}" "${RECORDED[5]}" | sort)"

echo "final snapshots:"
echo "$FINAL_IDS"
echo "expected (iterations 3, 4, 5):"
echo "$EXPECTED_IDS"

FINAL_COUNT="$(echo "$FINAL_IDS" | grep -c . || true)"
if [ "$FINAL_COUNT" -ne 3 ]; then
  echo "FAIL: expected exactly 3 surviving snapshots, got $FINAL_COUNT" >&2
  exit 1
fi
if [ "$FINAL_IDS" != "$EXPECTED_IDS" ]; then
  echo "FAIL: surviving snapshots are not exactly iterations 3, 4, 5" >&2
  exit 1
fi
echo "PASS: keep-last=3 kept exactly the 3 newest snapshots (iterations 3, 4, 5) and forgot 1 and 2"

# --- daemon: prune and check, via internal/daemon/maintenance.go -----------

log "daemon: prune + check smoke test (BALLAST_PRUNE_SCHEDULE / BALLAST_CHECK_SCHEDULE)"
docker rm -f "$DAEMON" >/dev/null 2>&1 || true
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@yearly" \
  -e BALLAST_PRUNE_SCHEDULE="@every 5s" \
  -e BALLAST_CHECK_SCHEDULE="@every 8s" \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

echo "waiting up to 60s for prune and check to each complete at least once..."
PRUNE_OK=0
CHECK_OK=0
for i in $(seq 1 30); do
  LOGS="$(docker logs "$DAEMON" 2>&1)"
  if echo "$LOGS" | grep -q 'msg="daemon: maintenance failed" action=prune'; then
    echo "FAIL: prune reported a failure" >&2
    echo "$LOGS" | tail -40 >&2
    exit 1
  fi
  if echo "$LOGS" | grep -q 'msg="daemon: maintenance failed" action=check'; then
    echo "FAIL: check reported a failure" >&2
    echo "$LOGS" | tail -40 >&2
    exit 1
  fi
  echo "$LOGS" | grep -q 'msg="daemon: maintenance completed" action=prune' && PRUNE_OK=1
  echo "$LOGS" | grep -q 'msg="daemon: maintenance completed" action=check' && CHECK_OK=1
  if [ "$PRUNE_OK" -eq 1 ] && [ "$CHECK_OK" -eq 1 ]; then
    break
  fi
  sleep 2
done

docker logs "$DAEMON" 2>&1 | tail -20

if [ "$PRUNE_OK" -ne 1 ]; then
  echo "FAIL: never saw a completed prune in daemon logs" >&2
  exit 1
fi
echo "PASS: daemon prune completed with no error"

if [ "$CHECK_OK" -ne 1 ]; then
  echo "FAIL: never saw a completed check in daemon logs" >&2
  exit 1
fi
echo "PASS: daemon check completed with no error (integrity OK)"

# Stop the daemon before touching the repo directly, so its own periodic
# prune/check can't race the explicit check --read-data run below (restic
# takes an exclusive lock for prune; a concurrent check would fail on it).
docker rm -f "$DAEMON" >/dev/null 2>&1 || true

# --- repo still valid after prune: snapshot count unchanged -----------------

log "confirming the repository is still valid after prune (snapshot count unchanged)"
POST_PRUNE_COUNT="$(all_snapshot_ids | grep -c . || true)"
if [ "$POST_PRUNE_COUNT" -ne 3 ]; then
  echo "FAIL: expected 3 snapshots after prune, got $POST_PRUNE_COUNT" >&2
  exit 1
fi
echo "PASS: 3 snapshots still present after prune"

# --- explicit check --read-data, direct against the repo -------------------
#
# The daemon's own CheckSchedule always calls Check with readData=false
# (internal/daemon/maintenance.go). To also prove --read-data, this shells
# out directly to the restic binary bundled in the same ballast:itest image,
# against the same repository, using the password "ballast key" derives
# (the same derivation path Ballast itself uses to build the repo password).

log "ballast key $SVC (for direct restic check --read-data)"
ballast key "$SVC" > "$PWFILE"
chmod 600 "$PWFILE"

log "restic check --read-data (direct, same restic binary as the engine)"
# NOT :ro: restic takes an exclusive repository lock for the duration of
# check (writing a lock file under $REPO/locks/) even though check itself
# never modifies snapshot or pack data. A read-only mount makes that lock
# write fail every retry, forever, hanging restic in an infinite backoff
# loop rather than surfacing a real error - caught by exactly that hang the
# first time this script ran.
docker run --rm --entrypoint restic \
  -e RESTIC_REPOSITORY="/repos/$SVC" \
  -e RESTIC_PASSWORD_FILE=/pw \
  -v "$REPOS":/repos \
  -v "$PWFILE":/pw:ro \
  "$IMAGE" check --read-data
echo "PASS: restic check --read-data reports integrity OK"

log "done"
