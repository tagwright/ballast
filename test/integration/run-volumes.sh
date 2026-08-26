#!/usr/bin/env bash
# Ballast multiple-volumes / narrowing / exclude integration test: proves
# internal/discovery/volumes.go's ballast.volumes / ballast.volumes.exclude
# narrowing and internal/discovery's ballast.exclude / ballast.exclude-caches
# label parsing actually change what lands in a restored snapshot, not just
# that they parse without error. Per docs/TESTING.md's coverage matrix, none
# of this was integration-proven before this script: only ballast.volumes=none
# (run-stream.sh) had ever been exercised end to end.
#
# Four one-shot backup/restore round trips, each a real "ballast backup" +
# "ballast restore" + byte-diff against known canary content:
#
#   1. A service with two named volumes and no narrowing label: both must be
#      backed up (both canaries restored).
#   2. The same two volumes, ballast.volumes=<one volume's name>: only that
#      volume is captured (the other canary must be absent from the restore).
#   3. The same two volumes, ballast.volumes.exclude=<one volume's name>: the
#      opposite of #2 (the excluded volume's canary must be absent, the other
#      present).
#   4. A single volume with a canary file, a *.log file excluded via
#      ballast.exclude=*.log, and a cache/ directory tagged with a real
#      CACHEDIR.TAG (the Cache Directory Tagging Specification's exact
#      signature line) excluded via ballast.exclude-caches=true: the canary
#      must survive the restore, the *.log file and the entire cache/
#      directory must not.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volumes) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-volumes.sh [--keep]
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

VOL_A=ballast-itest-vol-a
VOL_B=ballast-itest-vol-b
VOL_C=ballast-itest-vol-c
SVC_BOTH=ballast-itest-vols-both
SVC_ONLY_A=ballast-itest-vols-only-a
SVC_EXCL_A=ballast-itest-vols-excl-a
SVC_EXCLUDE=ballast-itest-vols-exclude
IMAGE=ballast:itest
CFG="$HARNESS_DIR/volumes.itest.yml"
REPOS="$HARNESS_DIR/repos-volumes"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-volumes"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC_BOTH" "$SVC_ONLY_A" "$SVC_EXCL_A" "$SVC_EXCLUDE" >/dev/null 2>&1 || true
  docker volume rm "$VOL_A" "$VOL_B" "$VOL_C" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into volumes.itest.yml (/var/lib/docker/volumes)." >&2
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

docker rm -f "$SVC_BOTH" "$SVC_ONLY_A" "$SVC_EXCL_A" "$SVC_EXCLUDE" >/dev/null 2>&1 || true
docker volume rm "$VOL_A" "$VOL_B" "$VOL_C" >/dev/null 2>&1 || true

# =============================================================================
# 1. Two volumes, no narrowing: both must be backed up.
# =============================================================================

log "creating $SVC_BOTH with two named volumes, no narrowing label"
docker run -d --name "$SVC_BOTH" \
  -v "$VOL_A":/vol-a \
  -v "$VOL_B":/vol-b \
  --label ballast.enable=true \
  --label ballast.repo=local \
  busybox sh -c '
    echo "canary-a-$(date +%s)" > /vol-a/canary-a.txt
    echo "canary-b-$(date +%s)" > /vol-b/canary-b.txt
    sleep 3600
  '
sleep 1
CANARY_A="$(docker exec "$SVC_BOTH" cat /vol-a/canary-a.txt)"
CANARY_B="$(docker exec "$SVC_BOTH" cat /vol-b/canary-b.txt)"

log "ballast backup $SVC_BOTH"
ballast backup "$SVC_BOTH"
ballast restore "$SVC_BOTH" --target /restore/both

FOUND_A="$(find "$RESTORE/both" -name canary-a.txt | head -1)"
FOUND_B="$(find "$RESTORE/both" -name canary-b.txt | head -1)"
if [ -z "$FOUND_A" ] || [ "$(cat "$FOUND_A")" != "$CANARY_A" ]; then
  echo "FAIL: canary-a.txt missing or mismatched in restore of $SVC_BOTH" >&2
  exit 1
fi
if [ -z "$FOUND_B" ] || [ "$(cat "$FOUND_B")" != "$CANARY_B" ]; then
  echo "FAIL: canary-b.txt missing or mismatched in restore of $SVC_BOTH" >&2
  exit 1
fi
echo "PASS: both volumes backed up and restored byte-for-byte with no narrowing label"

# =============================================================================
# 2. ballast.volumes=<VOL_A>: only volume A captured.
# =============================================================================

log "creating $SVC_ONLY_A with ballast.volumes=$VOL_A"
docker run -d --name "$SVC_ONLY_A" \
  -v "$VOL_A":/vol-a \
  -v "$VOL_B":/vol-b \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes="$VOL_A" \
  busybox sh -c 'sleep 3600'
sleep 1

log "ballast backup $SVC_ONLY_A"
ballast backup "$SVC_ONLY_A"
ballast restore "$SVC_ONLY_A" --target /restore/only-a

FOUND_A="$(find "$RESTORE/only-a" -name canary-a.txt | head -1)"
FOUND_B="$(find "$RESTORE/only-a" -name canary-b.txt | head -1)"
if [ -z "$FOUND_A" ]; then
  echo "FAIL: canary-a.txt missing from restore of $SVC_ONLY_A (ballast.volumes=$VOL_A should have kept it)" >&2
  exit 1
fi
if [ -n "$FOUND_B" ]; then
  echo "FAIL: canary-b.txt present in restore of $SVC_ONLY_A; ballast.volumes=$VOL_A should have narrowed volume B out" >&2
  exit 1
fi
echo "PASS: ballast.volumes=$VOL_A captured only volume A"

# =============================================================================
# 3. ballast.volumes.exclude=<VOL_A>: volume A excluded, volume B kept.
# =============================================================================

log "creating $SVC_EXCL_A with ballast.volumes.exclude=$VOL_A"
docker run -d --name "$SVC_EXCL_A" \
  -v "$VOL_A":/vol-a \
  -v "$VOL_B":/vol-b \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes.exclude="$VOL_A" \
  busybox sh -c 'sleep 3600'
sleep 1

log "ballast backup $SVC_EXCL_A"
ballast backup "$SVC_EXCL_A"
ballast restore "$SVC_EXCL_A" --target /restore/excl-a

FOUND_A="$(find "$RESTORE/excl-a" -name canary-a.txt | head -1)"
FOUND_B="$(find "$RESTORE/excl-a" -name canary-b.txt | head -1)"
if [ -n "$FOUND_A" ]; then
  echo "FAIL: canary-a.txt present in restore of $SVC_EXCL_A; ballast.volumes.exclude=$VOL_A should have dropped it" >&2
  exit 1
fi
if [ -z "$FOUND_B" ]; then
  echo "FAIL: canary-b.txt missing from restore of $SVC_EXCL_A (volume B was not excluded, it should be present)" >&2
  exit 1
fi
echo "PASS: ballast.volumes.exclude=$VOL_A dropped volume A and kept volume B"

# =============================================================================
# 4. ballast.exclude=*.log and ballast.exclude-caches=true.
# =============================================================================

log "creating $SVC_EXCLUDE with a canary, a *.log file, and a CACHEDIR.TAG-tagged cache dir"
docker run -d --name "$SVC_EXCLUDE" \
  -v "$VOL_C":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.exclude="*.log" \
  --label ballast.exclude-caches=true \
  busybox sh -c '
    mkdir -p /data/cache
    echo "keep-$(date +%s)" > /data/canary.txt
    echo "drop-me" > /data/debug.log
    printf "Signature: 8a477f597d28d172789f06886806bc55\n# this dir is machine-generated cache data\n" > /data/cache/CACHEDIR.TAG
    echo "drop-me-too" > /data/cache/some-cache-file.txt
    sleep 3600
  '
sleep 1

log "ballast backup $SVC_EXCLUDE"
ballast backup "$SVC_EXCLUDE"
ballast restore "$SVC_EXCLUDE" --target /restore/exclude

if [ -z "$(find "$RESTORE/exclude" -name canary.txt)" ]; then
  echo "FAIL: canary.txt missing from restore of $SVC_EXCLUDE" >&2
  exit 1
fi
echo "PASS: canary.txt survived the backup"

if [ -n "$(find "$RESTORE/exclude" -name '*.log')" ]; then
  echo "FAIL: a *.log file is present in the restore of $SVC_EXCLUDE; ballast.exclude=*.log did not exclude it" >&2
  find "$RESTORE/exclude" -name '*.log' >&2
  exit 1
fi
echo "PASS: ballast.exclude=*.log excluded debug.log"

# restic's --exclude-caches semantics (matching the CACHEDIR.TAG spec's own
# convention, also followed by rsync/borg/attic): a correctly tagged cache
# directory's CONTENTS are excluded, but the directory itself and the tag
# file are kept as a marker, so the directory can be recognized as a cache
# dir again without needing to be recreated. So the assertion here is that
# some-cache-file.txt (the actual cache payload) did not survive, not that
# the whole cache/ directory or CACHEDIR.TAG itself vanished.
if [ -n "$(find "$RESTORE/exclude" -name 'some-cache-file.txt')" ]; then
  echo "FAIL: cache/some-cache-file.txt survived the restore of $SVC_EXCLUDE; ballast.exclude-caches=true did not exclude the tagged directory's contents" >&2
  find "$RESTORE/exclude" -path '*cache*' >&2
  exit 1
fi
if [ -z "$(find "$RESTORE/exclude" -name 'CACHEDIR.TAG')" ]; then
  echo "FAIL: CACHEDIR.TAG itself is missing from the restore of $SVC_EXCLUDE; restic's --exclude-caches is documented to keep the tag file as a marker" >&2
  exit 1
fi
echo "PASS: ballast.exclude-caches=true excluded the tagged cache/ directory's contents (some-cache-file.txt), keeping the CACHEDIR.TAG marker itself, matching restic's documented behavior"

log "done"
