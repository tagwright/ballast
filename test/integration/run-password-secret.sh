#!/usr/bin/env bash
# Ballast password-secret override integration test: proves
# ballast.password-secret=<name> actually makes the repository password come
# from that named secret, not from the master-key HKDF derivation
# (internal/secret/derive.go's DeriveRepoPassword) every other service uses.
#
# It creates one labeled service with ballast.password-secret=svc-password
# (a secret present in the secrets dir, distinct from repo-master-key),
# proves a normal Ballast backup + restore round-trips through it end to
# end, and then proves the negative directly: the master-key-derived
# password (what "ballast key <service>" prints) does NOT open the
# repository, while the actual named secret's value does, both checked with
# the same restic binary Ballast itself bundles, run directly against the
# repository.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (container, volume) or tagged "ballast:itest" (image). It
# never touches any other container, volume, or image. Cleanup runs on exit
# (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-password-secret.sh [--keep]
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

SVC=ballast-itest-pwsecret-svc
VOL=ballast-itest-pwsecret-data
IMAGE=ballast:itest
CFG="$HARNESS_DIR/password-secret.itest.yml"
REPOS="$HARNESS_DIR/repos-password-secret"
SECRETS="$HARNESS_DIR/secrets-password-secret"
RESTORE="$HARNESS_DIR/restore-password-secret"
DERIVED_PWFILE="$HARNESS_DIR/password-secret-pw.tmp"
SECRET_PWFILE="$HARNESS_DIR/password-secret-actual-pw.tmp"
DERIVED_OUT="$HARNESS_DIR/password-secret-derived-out.tmp"
DERIVED_ERR="$HARNESS_DIR/password-secret-derived-err.tmp"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$SVC" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  rm -f "$DERIVED_PWFILE" "$SECRET_PWFILE" "$DERIVED_OUT" "$DERIVED_ERR" 2>/dev/null || true
  rm -rf "${REPOS:?}"/* "${RESTORE:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into password-secret.itest.yml (/var/lib/docker/volumes)." >&2
  echo "      Add a host_roots entry mapping $VOLUMES_ROOT to itself before running this harness." >&2
  exit 1
fi

mkdir -p "$REPOS" "$SECRETS" "$RESTORE"

# --- secrets -----------------------------------------------------------------
#
# repo-master-key is generated too (even though this service never uses it)
# so "ballast key" -- which only needs the master secret -- works the same
# way it would in a real deployment. svc-password is the secret
# ballast.password-secret names; its value is deliberately NOT derivable
# from the master key at all.

if [ ! -s "$SECRETS/repo-master-key" ]; then
  log "generating repo-master-key"
  openssl rand -base64 32 > "$SECRETS/repo-master-key"
fi
if [ ! -s "$SECRETS/svc-password" ]; then
  log "generating svc-password (the named secret ballast.password-secret points at)"
  openssl rand -base64 24 > "$SECRETS/svc-password"
fi

# --- build ---------------------------------------------------------------

log "building $IMAGE"
docker build -f "$REPO_ROOT/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

# --- test service: ballast.password-secret=svc-password ---------------------

log "creating $SVC (ballast.password-secret=svc-password) with canary data in $VOL"
docker rm -f "$SVC" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name "$SVC" \
  -v "$VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label ballast.password-secret=svc-password \
  busybox sh -c 'mkdir -p /data && echo "ballast-itest-pwsecret-canary-$(date +%s)" > /data/canary.txt && sleep 3600'

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

# --- backup + restore, exactly like run.sh -----------------------------------

log "ballast backup $SVC"
ballast backup "$SVC"

if [ -z "$(find "$REPOS" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
  echo "FAIL: repos directory is empty after backup" >&2
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

echo "original canary:  $ORIGINAL_CANARY"
echo "restored canary:  $RESTORED_CANARY"
if [ "$ORIGINAL_CANARY" != "$RESTORED_CANARY" ]; then
  echo "FAIL: restored canary does not match the original" >&2
  exit 1
fi
echo "PASS: backup + restore round-trip through ballast.password-secret works end to end"

# --- the actual proof: derived password must NOT open the repo --------------
#
# Both checks below shell out directly to the same restic binary Ballast's
# own image bundles, against the same repository, entirely independent of
# Ballast's own password resolution -- so a bug that made BuildRepo silently
# ignore ballast.password-secret and fall back to the derived password would
# still have passed the backup/restore round-trip above (since Ballast would
# then consistently use the derived password at both write and read time),
# but would fail the check below.

REPO_PATH="/repos/$SVC"

log "ballast key $SVC (the master-key-derived password, which must NOT work here)"
ballast key "$SVC" > "$DERIVED_PWFILE"
chmod 600 "$DERIVED_PWFILE"

# --no-lock: a plain "restic snapshots" still takes a (shared) repository
# lock by default, which means writing a lock file under <repo>/locks/ even
# though snapshots itself never modifies data. Against the :ro mount below
# that write fails and restic retries forever with growing backoff instead
# of producing a clean error -- exactly the read-only-mount hang
# docs/TESTING.md already documents from run-retention.sh's first draft.
# --no-lock sidesteps it entirely, which is also the right flag here on its
# own merits: this is a one-shot read-only check, not a real Ballast
# operation that needs real lock coordination.
log "confirming the derived password does NOT open the repository"
if docker run --rm --entrypoint restic \
    -e RESTIC_REPOSITORY="$REPO_PATH" \
    -e RESTIC_PASSWORD_FILE=/pw \
    -v "$REPOS":/repos:ro \
    -v "$DERIVED_PWFILE":/pw:ro \
    "$IMAGE" snapshots --json --no-lock >"$DERIVED_OUT" 2>"$DERIVED_ERR"; then
  echo "FAIL: the master-key-derived password opened the repository; ballast.password-secret is not actually overriding it" >&2
  cat "$DERIVED_ERR" >&2
  exit 1
fi
echo "PASS: the derived password was correctly rejected"
grep -qi "wrong password\|unable to open\|invalid password\|no key found" "$DERIVED_ERR" \
  && echo "  (restic reported a wrong-password-shaped error, as expected)"

log "confirming the actual named secret's value DOES open the repository"
cp "$SECRETS/svc-password" "$SECRET_PWFILE"
chmod 600 "$SECRET_PWFILE"
SNAP_OUT="$(docker run --rm --entrypoint restic \
  -e RESTIC_REPOSITORY="$REPO_PATH" \
  -e RESTIC_PASSWORD_FILE=/pw \
  -v "$REPOS":/repos:ro \
  -v "$SECRET_PWFILE":/pw:ro \
  "$IMAGE" snapshots --json --no-lock)"
SNAP_COUNT="$(echo "$SNAP_OUT" | grep -o '"id"' | grep -c . || true)"
if [ "$SNAP_COUNT" -eq 0 ]; then
  echo "FAIL: the actual svc-password secret did not open the repository (0 snapshots found)" >&2
  exit 1
fi
echo "PASS: the named secret's own value opens the repository directly ($SNAP_COUNT snapshot(s)), confirming it -- not the derived password -- is the real repository password"

log "done"
