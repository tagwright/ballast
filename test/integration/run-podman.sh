#!/usr/bin/env bash
# Ballast Podman-adapter integration test: the first real end-to-end
# exercise of pkg/runtime/podman.go against a live Podman socket, not just
# a compile check. Every prior itest run drove the Docker adapter only.
#
# Ballast has no test dependency on a Podman install on the host running
# this harness: it stands up its own throwaway, self-contained Podman by
# running quay.io/podman/stable (--privileged, needed for Podman's own
# nested container runtime) and starting Podman's Docker-compatible compat
# API service inside it. Two objects are shared between that nested-Podman
# container and the ballast:itest container purely via Docker-managed named
# volumes (never a host bind-mount path, since this harness itself may be
# running inside a container whose filesystem is not the Docker host's):
#
#   - ballast-itest-podman-sock: holds podman.sock, so both containers reach
#     the same live Podman API socket.
#   - ballast-itest-podman-volumes: mounted at Podman's own named-volume
#     data root (/var/lib/containers/storage/volumes) inside the nested
#     Podman container, and at the identical path (read-only) inside every
#     ballast:itest invocation, so a named volume's host-side Source path
#     Podman reports is a path Ballast can actually read -- exactly how the
#     real deploy's /var/lib/docker/volumes sharing works for Docker,
#     mirrored for Podman's different data root (see podman.itest.yml's
#     host_roots entry for that path translation).
#
# It proves, against the real socket:
#   1. Filesystem backup/restore against a local repo: a labeled container
#      inside the nested Podman, its named volume's canary file, a real
#      `ballast backup`/`snapshots`/`restore`, byte-diffed.
#   2. Compose-identity normalization actually exercised: the test
#      container is labeled ONLY with io.podman.compose.project/service
#      (no com.docker.compose.* pair at all), so a "project=..." tag
#      landing on the snapshot proves podmanComposeIdentity's
#      io.podman.compose.* fallback path, not merely its Docker-compat
#      preference branch.
#   3. The daemon's live watch loop against a real Podman event stream:
#      discovering a container via a real "start" event and firing a
#      scheduled backup for it, exactly as run-watch.sh proves for Docker.
#   4. A regression check for a real bug this pass found running against a
#      live socket for the first time: Podman's compat API emits "remove"
#      for container removal, never "destroy" (confirmed directly via curl
#      against the raw /v1.41/events endpoint before the fix). A container
#      already stopped when the daemon starts (discovered via the initial
#      List(All:true) pass, not a "start" event) and then removed produces
#      only a "remove" event, no "die" -- pkg/runtime/engine.go's
#      mapEventAction mapped only events.ActionDestroy before the fix, so
#      this removal was silently missed and the service's scheduled job
#      leaked forever. Fixed by mapping events.ActionRemove to EventDestroy
#      too; this step reproduces the exact scenario and asserts "daemon:
#      service unregistered" actually appears.
#
# Every object this script creates is named with the prefix
# "ballast-itest-" (Docker containers/volumes/image, and every Podman
# object created inside the nested Podman) or tagged "ballast:itest".
# Cleanup runs on exit (success, failure, or interrupt) via the trap below,
# removing objects in BOTH Docker and the nested Podman.
#
# Usage: test/integration/run-podman.sh [--keep]
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

PODMAN_HOST=ballast-itest-podman
SOCK_VOL=ballast-itest-podman-sock
VOLUMES_VOL=ballast-itest-podman-volumes
SVC=ballast-itest-svc
DATA_VOL=ballast-itest-data
DAEMON=ballast-itest-daemon-podman
IMAGE=ballast:itest
CFG="$HARNESS_DIR/podman.itest.yml"
REPOS="$HARNESS_DIR/repos-podman"
SECRETS="$HARNESS_DIR/secrets-podman"
RESTORE="$HARNESS_DIR/restore-podman"

log() { printf '\n=== %s ===\n' "$1"; }

podman_host() {
  docker exec "$PODMAN_HOST" podman "$@"
}

ballast_podman() {
  docker run --rm \
    -v "$SOCK_VOL":/run/podman-host \
    -v "$VOLUMES_VOL":/var/lib/containers/storage/volumes:ro \
    -v "$REPOS":/repos \
    -v "$SECRETS":/run/ballast/secrets:ro \
    -v "$CFG":/etc/ballast/ballast.yml:ro \
    -v "$RESTORE":/restore \
    -e BALLAST_RUNTIME=podman \
    -e BALLAST_SOCKET=/run/podman-host/podman.sock \
    "$IMAGE" "$@" --config /etc/ballast/ballast.yml
}

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done (both docker and: docker exec $PODMAN_HOST podman ...)"
    return
  fi
  log "cleanup"
  docker rm -f "$DAEMON" >/dev/null 2>&1 || true
  docker exec "$PODMAN_HOST" podman rm -f "$SVC" >/dev/null 2>&1 || true
  docker exec "$PODMAN_HOST" podman volume rm "$DATA_VOL" >/dev/null 2>&1 || true
  docker rm -f "$PODMAN_HOST" >/dev/null 2>&1 || true
  docker volume rm "$SOCK_VOL" "$VOLUMES_VOL" >/dev/null 2>&1 || true
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

# --- nested Podman sandbox -------------------------------------------------

log "standing up nested Podman ($PODMAN_HOST) with its own compat API socket"
docker rm -f "$PODMAN_HOST" >/dev/null 2>&1 || true
docker volume rm "$SOCK_VOL" "$VOLUMES_VOL" >/dev/null 2>&1 || true
docker volume create "$SOCK_VOL" >/dev/null
docker volume create "$VOLUMES_VOL" >/dev/null

docker run -d --name "$PODMAN_HOST" \
  --privileged \
  -v "$SOCK_VOL":/run/podman-host \
  -v "$VOLUMES_VOL":/var/lib/containers/storage/volumes \
  quay.io/podman/stable:latest \
  sleep infinity >/dev/null

log "waiting for Podman inside $PODMAN_HOST to be ready"
for i in $(seq 1 30); do
  if docker exec "$PODMAN_HOST" podman version >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
podman_host version

log "starting Podman's compat API service on the shared socket"
docker exec -d "$PODMAN_HOST" podman system service --time=0 unix:///run/podman-host/podman.sock
for i in $(seq 1 30); do
  if docker exec "$PODMAN_HOST" podman --url unix:///run/podman-host/podman.sock version >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$PODMAN_HOST" podman --url unix:///run/podman-host/podman.sock version >/dev/null

# --- test service ------------------------------------------------------------
#
# Labeled with io.podman.compose.* only, no com.docker.compose.* pair at
# all: proves podmanComposeIdentity's Podman-native fallback path, not just
# its Docker-compat-label preference branch (see the doc comment above).

log "creating $SVC inside Podman with canary data in $DATA_VOL"
podman_host rm -f "$SVC" >/dev/null 2>&1 || true
podman_host volume rm "$DATA_VOL" >/dev/null 2>&1 || true
podman_host volume create "$DATA_VOL" >/dev/null
podman_host run -d --name "$SVC" \
  -v "$DATA_VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label io.podman.compose.project=ballast-itest \
  --label io.podman.compose.service="$SVC" \
  docker.io/library/busybox sh -c 'mkdir -p /data && echo "ballast-itest-canary-$(date +%s)" > /data/canary.txt && sleep 3600' >/dev/null

sleep 1
ORIGINAL_CANARY="$(podman_host exec "$SVC" cat /data/canary.txt)"
echo "canary: $ORIGINAL_CANARY"

# --- backup ------------------------------------------------------------------

log "ballast backup $SVC (BALLAST_RUNTIME=podman)"
ballast_podman backup "$SVC"

if [ -z "$(find "$REPOS" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
  echo "FAIL: repos-podman directory is empty after backup" >&2
  exit 1
fi

# --- snapshots: assert the compose-identity normalization landed -----------

log "ballast snapshots $SVC (asserting io.podman.compose.* -> project tag)"
SNAP_OUT="$(ballast_podman snapshots "$SVC")"
echo "$SNAP_OUT"
if ! echo "$SNAP_OUT" | grep -q "project=ballast-itest"; then
  echo "FAIL: expected a project=ballast-itest tag from the io.podman.compose.* fallback, not found" >&2
  exit 1
fi
echo "PASS: snapshot tags carry project=ballast-itest via the io.podman.compose.* fallback"

# --- restore -------------------------------------------------------------

log "ballast restore $SVC"
ballast_podman restore "$SVC" --target /restore

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
echo "PASS: restored canary byte-matches the original, against a real Podman socket"

# --- daemon watch: EventStart discovery -------------------------------------

log "daemon watch smoke test: discover a container created after the daemon starts"
podman_host rm -f "$SVC" >/dev/null 2>&1 || true

docker rm -f "$DAEMON" >/dev/null 2>&1 || true
docker run -d --name "$DAEMON" \
  -v "$SOCK_VOL":/run/podman-host \
  -v "$VOLUMES_VOL":/var/lib/containers/storage/volumes:ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  -e BALLAST_RUNTIME=podman \
  -e BALLAST_SOCKET=/run/podman-host/podman.sock \
  -e BALLAST_SCHEDULE="@every 30s" \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml >/dev/null
sleep 2

podman_host run -d --name "$SVC" \
  -v "$DATA_VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  --label io.podman.compose.project=ballast-itest \
  --label io.podman.compose.service="$SVC" \
  docker.io/library/busybox sh -c 'echo watch-canary > /data/canary.txt && sleep 3600' >/dev/null

echo "waiting up to 60s for the daemon to discover it via a Podman 'start' event and back it up..."
FOUND=0
for i in $(seq 1 30); do
  if docker logs "$DAEMON" 2>&1 | grep -q "Backup OK: $SVC"; then
    FOUND=1
    break
  fi
  sleep 2
done
docker logs "$DAEMON" 2>&1 | tail -10
if [ "$FOUND" -ne 1 ]; then
  echo "FAIL: daemon never backed up a container discovered via a live Podman start event" >&2
  exit 1
fi
echo "PASS: daemon discovered and backed up a container via a real Podman socket event"

# --- regression check: EventDestroy via Podman's "remove" (not "die") ------
#
# See the doc comment at the top of this file: a container already stopped
# when the daemon starts (found via the initial List(All:true) pass, no
# "start" event involved) and then removed fires only a "remove" event,
# never "die". Before this pass's fix in pkg/runtime/engine.go, that event
# was silently unmapped and the service's scheduled job never dropped.

log "regression check: stopped-then-removed container fires 'remove' with no 'die'"
docker rm -f "$DAEMON" >/dev/null 2>&1 || true
podman_host rm -f "$SVC" >/dev/null 2>&1 || true

podman_host run -d --name "$SVC" \
  -v "$DATA_VOL":/data \
  --label ballast.enable=true \
  --label ballast.repo=local \
  docker.io/library/busybox sh -c 'sleep 3600' >/dev/null
sleep 1
podman_host stop -t 2 "$SVC" >/dev/null 2>&1 || true

docker run -d --name "$DAEMON" \
  -v "$SOCK_VOL":/run/podman-host \
  -v "$VOLUMES_VOL":/var/lib/containers/storage/volumes:ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  -e BALLAST_RUNTIME=podman \
  -e BALLAST_SOCKET=/run/podman-host/podman.sock \
  -e BALLAST_SCHEDULE="@daily" \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml >/dev/null
sleep 3

podman_host rm "$SVC"
sleep 3

DAEMON_LOG="$(docker logs "$DAEMON" 2>&1)"
echo "$DAEMON_LOG"
if ! echo "$DAEMON_LOG" | grep -q "daemon: service unregistered.*event=destroy"; then
  echo "FAIL: expected 'daemon: service unregistered ... event=destroy' after removing an already-stopped container (Podman's 'remove'-only event), got none" >&2
  exit 1
fi
echo "PASS: Podman's 'remove' event (no accompanying 'die') correctly unregisters the service"

log "done"
