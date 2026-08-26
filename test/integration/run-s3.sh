#!/usr/bin/env bash
# Ballast S3 object-storage backend integration test: proves the restic S3
# code path against a local MinIO standing in for Cloudflare R2 (real R2 is
# out of scope here, no credentials, and R2 is just an S3-compatible
# endpoint restic talks to the same way). It exercises the identical
# destination.env -> child-process-env -> restic S3 credential path a real
# R2 destination uses.
#
# It creates a user network, starts a throwaway MinIO server on it, creates
# a bucket, starts a throwaway labeled service (busybox with canary data in
# a volume, ballast.repo=s3test), runs ballast backup/restore attached to
# that network so it resolves the MinIO container by name, confirms objects
# landed in the MinIO bucket, and diffs the restored canary against the
# original.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volumes, network) or tagged "ballast:itest"
# (image). It never touches any other container, volume, network, or image.
# Cleanup runs on exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-s3.sh [--keep]
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

NET=ballast-itest-net
MINIO=ballast-itest-minio
SVC=ballast-itest-s3-svc
VOL=ballast-itest-s3-data
IMAGE=ballast:itest
CFG="$HARNESS_DIR/s3.itest.yml"
SECRETS="$HARNESS_DIR/secrets-s3"
RESTORE="$HARNESS_DIR/restore-s3"

MINIO_ROOT_USER=itestkey
MINIO_ROOT_PASSWORD=itestsecret123
BUCKET=ballast-itest

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker rm -f "$MINIO" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
echo "DockerRootDir: $DOCKER_ROOT"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into s3.itest.yml (/var/lib/docker/volumes)." >&2
  echo "      Add a host_roots entry mapping $VOLUMES_ROOT to itself before running this harness." >&2
  exit 1
fi

mkdir -p "$SECRETS" "$RESTORE"

# --- secrets -----------------------------------------------------------------

if [ ! -s "$SECRETS/repo-master-key" ]; then
  log "generating repo-master-key"
  openssl rand -base64 32 > "$SECRETS/repo-master-key"
fi
printf '%s' "$MINIO_ROOT_USER" > "$SECRETS/r2-access-key-id"
printf '%s' "$MINIO_ROOT_PASSWORD" > "$SECRETS/r2-secret-access-key"

# --- build ---------------------------------------------------------------

log "building $IMAGE"
docker build -f "$REPO_ROOT/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

# --- network + MinIO ---------------------------------------------------------

log "creating $NET"
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null

log "starting $MINIO"
docker rm -f "$MINIO" >/dev/null 2>&1 || true
docker run -d --name "$MINIO" \
  --network "$NET" \
  -e MINIO_ROOT_USER="$MINIO_ROOT_USER" \
  -e MINIO_ROOT_PASSWORD="$MINIO_ROOT_PASSWORD" \
  minio/minio server /data

log "creating bucket $BUCKET"
ready=0
for i in $(seq 1 60); do
  if docker run --rm --network "$NET" --entrypoint sh minio/mc \
      -c "mc alias set itest http://$MINIO:9000 $MINIO_ROOT_USER $MINIO_ROOT_PASSWORD >/dev/null 2>&1 && mc mb itest/$BUCKET" \
      >/dev/null 2>&1; then
    echo "bucket ready after ${i}s"
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "FAIL: MinIO did not become ready / bucket creation failed in time" >&2
  docker logs "$MINIO" 2>&1 | tail -40
  exit 1
fi

# --- test service ------------------------------------------------------------

log "creating $SVC with canary data in $VOL"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=s3test \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-s3-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

sleep 1
ORIGINAL_CANARY="$(docker exec "$SVC" cat /data/canary.txt)"
echo "canary: $ORIGINAL_CANARY"

ballast() {
  docker run --rm \
    --network "$NET" \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# --- backup ------------------------------------------------------------------

log "ballast backup $SVC (destination s3test / MinIO)"
ballast backup "$SVC"

# --- confirm objects landed in the MinIO bucket -------------------------------

log "checking MinIO bucket for objects"
OBJ_COUNT="$(docker run --rm --network "$NET" --entrypoint sh minio/mc \
  -c "mc alias set itest http://$MINIO:9000 $MINIO_ROOT_USER $MINIO_ROOT_PASSWORD >/dev/null 2>&1 && mc ls --recursive itest/$BUCKET" \
  | grep -c . || true)"
echo "objects in bucket: $OBJ_COUNT"
if [ "$OBJ_COUNT" -eq 0 ]; then
  echo "FAIL: no objects found in MinIO bucket $BUCKET after backup" >&2
  exit 1
fi
echo "PASS: backup wrote objects into the MinIO bucket"

# --- snapshots ---------------------------------------------------------------

log "ballast snapshots $SVC"
SNAP_OUT="$(ballast snapshots "$SVC")"
echo "$SNAP_OUT"
if [ "$(echo "$SNAP_OUT" | tail -n +2 | grep -c .)" -eq 0 ]; then
  echo "FAIL: ballast snapshots reported none from the S3 destination" >&2
  exit 1
fi
echo "PASS: ballast snapshots lists the snapshot from S3"

# --- restore -------------------------------------------------------------

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
echo "PASS: restored canary byte-matches the original, from the S3 (MinIO) backend"

log "done"
