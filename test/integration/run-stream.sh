#!/usr/bin/env bash
# Ballast stream (exec-to-restic-stdin) integration test: the second real
# end-to-end exercise of Ballast against a live Docker socket, proving the
# docker-exec-stdout-piped-into-restic-stdin path, the exec gate
# (BALLAST_ENABLE_EXEC), and stream snapshot tagging.
#
# It builds the image, starts a throwaway Postgres container with a known
# canary row, runs `ballast backup` with a ballast.stream.db.* label set
# (and ballast.volumes=none, so the live datadir is never touched), restores
# the resulting stream snapshot, and diffs the restored dump against the
# original CREATE TABLE + canary row. It then repeats the setup with a
# bogus dump command against a second throwaway container and confirms the
# run aborts WITHOUT leaving a snapshot behind.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers, volumes) or tagged "ballast:itest" (image).
# It never touches any other container, volume, or image. Cleanup runs on
# exit (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-stream.sh [--keep]
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

PG=ballast-itest-pg
PG_BAD=ballast-itest-pg-bad
IMAGE=ballast:itest
CFG="$HARNESS_DIR/stream.itest.yml"
REPOS="$HARNESS_DIR/repos-stream"
SECRETS="$HARNESS_DIR/secrets"
RESTORE="$HARNESS_DIR/restore-stream"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$PG" >/dev/null 2>&1 || true
  docker rm -f "$PG_BAD" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

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
    -e BALLAST_ENABLE_EXEC=true \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

# --- Postgres with a known canary row ---------------------------------------

log "starting $PG"
docker rm -f "$PG" >/dev/null 2>&1 || true
docker run -d --name "$PG" \
  -e POSTGRES_PASSWORD=itest \
  -e POSTGRES_DB=itest \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes=none \
  --label ballast.stream.db.command="pg_dump -U postgres itest" \
  --label ballast.stream.db.filename=itest.sql \
  --label ballast.stream.db.user=postgres \
  postgres:16

log "waiting for $PG to accept connections"
# The official postgres image's entrypoint runs initdb, starts a transient
# Unix-socket-only server to run init scripts, stops it, then starts the
# real server. pg_isready can report ready during that transient window, so
# readiness is confirmed by actually running a statement, retried until it
# succeeds rather than trusting a single pg_isready probe.
CANARY="ballast-itest-pg-canary-$(date +%s)"
echo "canary: $CANARY"
ready=0
for i in $(seq 1 60); do
  if docker exec "$PG" psql -U postgres -d itest -v ON_ERROR_STOP=1 -c \
      "CREATE TABLE t(v text); INSERT INTO t VALUES('$CANARY');" >/dev/null 2>&1; then
    echo "postgres ready and canary row written after ${i}s"
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "FAIL: postgres did not become ready in time" >&2
  docker logs "$PG" 2>&1 | tail -40
  exit 1
fi

# --- backup: real stream dump -------------------------------------------------

log "ballast backup $PG (stream dump)"
ballast backup "$PG"

log "ballast snapshots $PG"
SNAP_OUT="$(ballast snapshots "$PG")"
echo "$SNAP_OUT"
if ! echo "$SNAP_OUT" | grep -q "stream=db"; then
  echo "FAIL: no snapshot tagged stream=db found for $PG" >&2
  exit 1
fi
echo "PASS: stream snapshot landed, tagged stream=db"

# --- restore and diff ---------------------------------------------------------

log "ballast restore $PG"
ballast restore "$PG" --target /restore

RESTORED_FILE="$(find "$RESTORE" -name itest.sql | head -1)"
if [ -z "$RESTORED_FILE" ]; then
  echo "FAIL: itest.sql not found under $RESTORE after restore" >&2
  exit 1
fi

if ! grep -q "CREATE TABLE" "$RESTORED_FILE"; then
  echo "FAIL: restored dump does not contain CREATE TABLE" >&2
  exit 1
fi
if ! grep -qF "$CANARY" "$RESTORED_FILE"; then
  echo "FAIL: restored dump does not contain the canary row" >&2
  exit 1
fi
echo "PASS: restored dump contains CREATE TABLE and the canary row"

# --- failure path: bogus dump command must not write a snapshot -------------

log "starting $PG_BAD with a bogus stream command"
docker rm -f "$PG_BAD" >/dev/null 2>&1 || true
docker run -d --name "$PG_BAD" \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.volumes=none \
  --label ballast.stream.bad.command=false \
  --label ballast.stream.bad.filename=bad.txt \
  busybox sh -c 'sleep 3600'
sleep 1

log "ballast backup $PG_BAD (expected to fail)"
if ballast backup "$PG_BAD"; then
  echo "FAIL: backup of $PG_BAD succeeded despite a bogus dump command" >&2
  exit 1
fi
echo "PASS: backup aborted (non-zero exit) as expected"

log "ballast snapshots $PG_BAD"
BAD_SNAP_OUT="$(ballast snapshots "$PG_BAD")"
echo "$BAD_SNAP_OUT"
BAD_SNAP_COUNT="$(echo "$BAD_SNAP_OUT" | tail -n +2 | grep -c . || true)"
if [ "$BAD_SNAP_COUNT" -ne 0 ]; then
  echo "FAIL: expected 0 snapshots for $PG_BAD after a failed dump, got $BAD_SNAP_COUNT" >&2
  exit 1
fi
echo "PASS: no snapshot was left behind by the failed dump"

log "done"
