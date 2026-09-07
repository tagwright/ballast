#!/usr/bin/env bash
# Ballast EnsureRepo auto-init guard integration test: proves, end to end
# against a real restic repository, that EnsureRepo's would-init guard
# (Repo.GuardInit) both auto-initializes a genuinely new service and refuses
# to re-initialize when the guard says no, leaving no fresh repository behind.
# This is the regression safeguard from task #231: a destination that has
# vanished for a service that has backed up before must fail loudly rather than
# be silently re-created empty.
#
# Like run-retention-time.sh, this is deliberately NOT a Docker-container itest
# like the other run-*.sh scripts here: it runs at the ENGINE level.
# internal/engine/ensurerepo_guard_itest_test.go (build-tag "integration", so
# it never runs under a plain "go test ./...") drives engine.Restic.EnsureRepo
# against a real restic binary and asserts, for both branches, whether the
# repository ends up initialized.
#
# It runs that Go test inside a throwaway container (golang:1.25 plus a pinned
# restic binary, matching the version this repo's own Dockerfile bundles)
# rather than assuming the host has a matching restic on PATH. That container is
# the only Docker object this script creates, named
# "ballast-itest-ensurerepo-guard-runner"; it never touches the Docker socket,
# any other container, volume, network, or image, and is removed whether the
# test passes or fails (--rm).
#
# Usage: test/integration/run-ensurerepo-guard.sh
#   (no --keep: there is nothing here to inspect afterward except test
#   output, which this script already prints in full)

set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/../.." && pwd)"

RUNNER=ballast-itest-ensurerepo-guard-runner

# Same restic release and checksum the production Dockerfile pins, so this test
# exercises the identical restic version Ballast actually ships.
RESTIC_VERSION=0.19.1
RESTIC_SHA256=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c

log() { printf '\n=== %s ===\n' "$1"; }

cleanup() {
  log "cleanup"
  docker rm -f "$RUNNER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "running TestEnsureRepoGuard in a throwaway golang:1.25 + restic container"
docker run --rm --name "$RUNNER" \
  -e RESTIC_VERSION="$RESTIC_VERSION" \
  -e RESTIC_SHA256="$RESTIC_SHA256" \
  -v "$REPO_ROOT":/src:ro \
  -w /tmp/build \
  golang:1.25 \
  bash -euo pipefail -c '
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends bzip2 ca-certificates wget >/dev/null
    wget -q -O /tmp/restic.bz2 "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2"
    echo "${RESTIC_SHA256}  /tmp/restic.bz2" | sha256sum -c -
    bunzip2 /tmp/restic.bz2
    mv /tmp/restic /usr/local/bin/restic
    chmod +x /usr/local/bin/restic
    restic version

    cp -r /src /tmp/build/src
    cd /tmp/build/src
    go test -buildvcs=false -tags integration ./internal/engine/... -run TestEnsureRepoGuard -v -count=1
  '

echo "PASS: TestEnsureRepoGuard (EnsureRepo auto-init guard, at the engine level)"

log "done"
