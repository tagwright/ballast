#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 techgaud
#
# The live integration harness aggregator (tagwright Testing Standard, task
# #548). Runs the core end-to-end path (run.sh) and every scenario script in
# sequence against a live Docker socket, and fails on the first failure so one
# CI run surfaces the break. This is the single entry point the self-hosted CI
# leg (ci-selfhosted.yml) and the release gate (release.yml) both call, so "the
# harness" means one thing in one place rather than a drifting list per caller.
#
# All scenarios here drive the SHIPPED image (ballast:itest, built by run.sh
# and reused) against throwaway backends (MinIO for S3, a scratch sftp
# container, nested Podman): every one is socket-reachable, so every one is a
# CI gate, not operator-run. The only operator-run, LAST-RUN-attested paths are
# the permanent-exempt bucket (a real R2 bucket, real Vanta) named in
# docs/TESTING.md; none of them is a scenario script here.
#
# Usage: test/integration/run-all.sh [--keep]
#   --keep  pass through to each scenario (skip cleanup, for debugging)
set -uo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

KEEP=""
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP="--keep" ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

# run.sh is the base end-to-end path (backup -> snapshots -> restore -> daemon)
# and must pass first; the rest are ordered by cost, cheapest first.
SCENARIOS=(
  run.sh
  run-alias.sh
  run-conflict.sh
  run-dupe.sh
  run-ensurerepo-guard.sh
  run-volumes.sh
  run-repo-path.sh
  run-password-secret.sh
  run-stop.sh
  run-hooks.sh
  run-stream.sh
  run-watch.sh
  run-splay.sh
  run-retention.sh
  run-retention-time.sh
  run-notify.sh
  run-s3.sh
  run-sftp.sh
  run-podman.sh
)

fail=0
failed_list=""
for scen in "${SCENARIOS[@]}"; do
  printf '\n########## harness: %s ##########\n' "$scen"
  if ! "$HARNESS_DIR/$scen" $KEEP; then
    echo "harness: SCENARIO FAILED: $scen" >&2
    fail=1
    failed_list="$failed_list $scen"
    # keep going so one run reports every break
  fi
done

echo
if [ "$fail" -ne 0 ]; then
  echo "harness: FAIL (scenarios:$failed_list)" >&2
  exit 1
fi
echo "harness: all ${#SCENARIOS[@]} scenarios passed"
