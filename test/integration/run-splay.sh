#!/usr/bin/env bash
# Ballast multiple-services / splay / concurrency integration test.
#
# The splay-slot-distinctness half of this claim (that a fleet of services
# all set to the same period alias, e.g. @daily, land on different fnv-splay
# slots) is proven by internal/schedule/schedule_test.go's
# TestDailySplayDistinctAcrossThreeServices, a real @daily wait being
# impractical for an itest per docs/TESTING.md. What that unit test cannot
# prove is the live concurrency serialization: that a Scheduler with
# Concurrency=1 (the grammar's default) truly never runs two services' full
# backup lifecycles at once, even when several jobs come due at the same
# instant.
#
# This script proves that half against a real daemon: three labeled
# services (ballast.volumes=none, so there is no filesystem data to move --
# only the concurrency behavior matters here), each with an
# exec.pre="sleep 5" hook (gated by BALLAST_ENABLE_EXEC) writing a
# whole-second start marker into a shared bind-mounted host directory, and
# an exec.post writing an end marker the same way. All three
# fire on the daemon's global "@every 25s" default at (as close as
# ConstantDelaySchedule computes it) the same instant. Reading the three
# services' [start, end] intervals back from the marker files (whole
# seconds -- busybox's date has no %N) and confirming no two intervals
# overlap is a direct, data-level proof that Concurrency=1 actually
# serializes them, exercising the semaphore in
# internal/schedule/scheduler.go's dispatch, not just reading the code. The
# 5s exec.pre sleep makes each interval span several seconds, so
# whole-second granularity is more than enough resolution to catch a real
# overlap; the overlap check below uses a strict "<" specifically so two
# services whose boundary lands in the same second (one ending exactly when
# the next starts) is never flagged as a false positive.
#
# Every Docker object this script creates is named with the prefix
# "ballast-itest-" (containers) or tagged "ballast:itest" (image). It never
# touches any other container, volume, or image. Cleanup runs on exit
# (success, failure, or interrupt) via the trap below.
#
# Usage: test/integration/run-splay.sh [--keep]
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

SVC_A=ballast-itest-splay-a
SVC_B=ballast-itest-splay-b
SVC_C=ballast-itest-splay-c
DAEMON=ballast-itest-splay-daemon
IMAGE=ballast:itest
CFG="$HARNESS_DIR/splay.itest.yml"
REPOS="$HARNESS_DIR/repos-splay"
SECRETS="$HARNESS_DIR/secrets"
MARKERS="$HARNESS_DIR/markers-splay"

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    log "skipping cleanup (--keep); remove ballast-itest-* by hand when done"
    return
  fi
  log "cleanup"
  docker rm -f "$DAEMON" "$SVC_A" "$SVC_B" "$SVC_C" >/dev/null 2>&1 || true
  rm -rf "${REPOS:?}"/* "${MARKERS:?}"/* 2>/dev/null || true
}
trap cleanup EXIT

# --- host layout ------------------------------------------------------------

log "checking Docker volumes root"
DOCKER_ROOT="$(docker info --format '{{.DockerRootDir}}')"
VOLUMES_ROOT="$DOCKER_ROOT/volumes"
if [ "$VOLUMES_ROOT" != "/var/lib/docker/volumes" ]; then
  echo "NOTE: volumes root differs from the default baked into splay.itest.yml (/var/lib/docker/volumes)." >&2
  echo "      Add a host_roots entry mapping $VOLUMES_ROOT to itself before running this harness." >&2
  exit 1
fi

mkdir -p "$REPOS" "$SECRETS" "$MARKERS"
rm -f "$MARKERS"/*.start "$MARKERS"/*.end 2>/dev/null || true

# --- secrets -----------------------------------------------------------------

if [ ! -s "$SECRETS/repo-master-key" ]; then
  log "generating repo-master-key"
  openssl rand -base64 32 > "$SECRETS/repo-master-key"
fi

# --- build ---------------------------------------------------------------

log "building $IMAGE"
docker build -f "$REPO_ROOT/Dockerfile" -t "$IMAGE" "$REPO_ROOT"

# --- three services, same schedule (daemon default), artificially slow -----

docker rm -f "$DAEMON" "$SVC_A" "$SVC_B" "$SVC_C" >/dev/null 2>&1 || true

for SVC in "$SVC_A" "$SVC_B" "$SVC_C"; do
  log "creating $SVC (ballast.volumes=none, exec.pre sleeps 5s and marks start/end)"
  docker run -d --name "$SVC" \
    -v "$MARKERS":/markers \
    --label ballast.enable=true \
    --label ballast.repo=local \
    --label ballast.volumes=none \
    --label ballast.exec.pre="date -u +%s >> /markers/$SVC.start && sleep 5" \
    --label ballast.exec.post="date -u +%s >> /markers/$SVC.end" \
    busybox sh -c 'sleep 3600'
done

log "starting $DAEMON (BALLAST_SCHEDULE=@every 25s, BALLAST_ENABLE_EXEC=true, default Concurrency=1)"
docker run -d --name "$DAEMON" \
  -e BALLAST_SCHEDULE="@every 25s" \
  -e BALLAST_ENABLE_EXEC=true \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v "$VOLUMES_ROOT":"$VOLUMES_ROOT":ro \
  -v "$REPOS":/repos \
  -v "$SECRETS":/run/ballast/secrets:ro \
  -v "$CFG":/etc/ballast/ballast.yml:ro \
  "$IMAGE" daemon --config /etc/ballast/ballast.yml

# --- wait for all three rounds of markers ------------------------------------

log "waiting up to 90s for all three services' start+end markers"
READY=0
for i in $(seq 1 45); do
  COUNT=0
  for SVC in "$SVC_A" "$SVC_B" "$SVC_C"; do
    [ -s "$MARKERS/$SVC.start" ] && [ -s "$MARKERS/$SVC.end" ] && COUNT=$((COUNT + 1))
  done
  if [ "$COUNT" -eq 3 ]; then
    READY=1
    break
  fi
  sleep 2
done
if [ "$READY" -ne 1 ]; then
  echo "FAIL: not all three services produced both markers within 90s" >&2
  ls -la "$MARKERS" >&2 || true
  docker logs "$DAEMON" 2>&1 | tail -60
  exit 1
fi
echo "PASS: all three services ran their exec.pre/exec.post hooks"

# --- prove serialization: no two [start, end] intervals overlap -------------

log "confirming no two services' backup intervals overlap (Concurrency=1)"
declare -A START END
for SVC in "$SVC_A" "$SVC_B" "$SVC_C"; do
  # head -n1: the daemon may already be into a second @every 25s round for
  # some services by the time this reads (dispatch order among
  # simultaneously-ready jobs is not FIFO -- see the race note above these
  # loops), so the marker files use >> (append) rather than truncation and
  # this always reads the FIRST line, i.e. round 1's values, which are
  # never overwritten by any later round.
  START[$SVC]="$(head -n1 "$MARKERS/$SVC.start")"
  END[$SVC]="$(head -n1 "$MARKERS/$SVC.end")"
  SPAN_S=$(( ${END[$SVC]} - ${START[$SVC]} ))
  echo "$SVC: start=${START[$SVC]} end=${END[$SVC]} (span ${SPAN_S}s)"
  if [ "${END[$SVC]}" -le "${START[$SVC]}" ]; then
    echo "FAIL: $SVC's end marker is not after its start marker" >&2
    exit 1
  fi
done

OVERLAP=0
SERVICES=("$SVC_A" "$SVC_B" "$SVC_C")
for ((i = 0; i < ${#SERVICES[@]}; i++)); do
  for ((j = i + 1; j < ${#SERVICES[@]}; j++)); do
    A="${SERVICES[$i]}"; B="${SERVICES[$j]}"
    if [ "${START[$A]}" -lt "${END[$B]}" ] && [ "${START[$B]}" -lt "${END[$A]}" ]; then
      echo "FAIL: $A [${START[$A]}, ${END[$A]}] overlaps $B [${START[$B]}, ${END[$B]}]" >&2
      OVERLAP=1
    fi
  done
done
if [ "$OVERLAP" -ne 0 ]; then
  echo "FAIL: two services' backup runs overlapped despite Concurrency=1" >&2
  exit 1
fi
echo "PASS: all three services' backup runs are strictly non-overlapping (Concurrency=1 serialized them)"

# --- splay slot distinctness: see internal/schedule/schedule_test.go --------

log "splay-slot distinctness for these exact service names is proven separately"
echo "See internal/schedule/schedule_test.go's TestDailySplayDistinctAcrossThreeServices,"
echo "which computes real @daily slots for $SVC_A/$SVC_B/$SVC_C and asserts they are"
echo "pairwise distinct -- a real @daily wait is impractical for a live itest per docs/TESTING.md."

log "done"
