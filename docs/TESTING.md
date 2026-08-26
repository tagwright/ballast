# Testing

This is Ballast's test methodology and an honest accounting of what has
actually been proven to work, as opposed to what merely compiles.

## How we test

Three layers, in increasing order of how much they actually prove:

1. **Unit tests, in-tree** (`go test ./...`, run in CI). Pure-function
   coverage: label parsing, path/glob logic, the scheduler's cron and splay
   math. Fast, no Docker socket, no external services. Narrow by design:
   most of Ballast's packages talk directly to a live Docker socket or shell
   out to `restic`, and a unit test around those would either need a fake
   that drifts from the real API or wouldn't test anything real. See
   `internal/discovery/*_test.go` and `internal/schedule/*_test.go` for what
   exists today.

2. **`test/integration/` harness, against a live Docker socket.** This is
   where Ballast is actually proven end to end: build the real image from
   the real Dockerfile, run real throwaway containers, drive the real
   `ballast` binary through a real backup and restore against a real restic
   repository, and diff real bytes. Every object these scripts create is
   named `ballast-itest-*` (or tagged `ballast:itest`), never touches
   anything else on the host, and is torn down in a trap on exit (success,
   failure, or interrupt). Three scripts today:

   - `run.sh` — filesystem backup/restore against a local repo, plus a
     daemon scheduler smoke test (`@every 1m`, confirmed to fire a second
     scheduled backup on its own).
   - `run-stream.sh` — the docker-exec-stdout-piped-into-`restic --stdin`
     path: a real Postgres container, a `ballast.stream.db.*` labeled dump,
     restore, and a failure-path check that a bogus dump command leaves no
     snapshot behind.
   - `run-s3.sh` — the restic S3 backend against a local MinIO standing in
     for Cloudflare R2 (see the R2 note below), including the
     `destination.env` → child-process-env → restic credential path.

   See `test/integration/README.md` for how to run each one.

3. **Deliberately manual.** A few things are exercised by hand rather than
   by an automated harness, noted individually in the matrix below — mostly
   because they need a real external account (Cloudflare R2) or a real
   notification backend's live endpoint (an actual ntfy/Discord/SMTP
   target), neither of which belongs in a repo-local test run.

## Coverage matrix

Categories, precisely:

- **Integration-proven** — a `test/integration/*.sh` run has actually
  exercised this against live Docker, a real `restic` binary, and (where
  relevant) a real backend, and asserted a real outcome (byte-for-byte
  restore, an object landing in a bucket, a run aborting correctly).
- **Unit-tested** — covered by `go test`, no live socket or external
  service involved.
- **Compile-only** — the code builds and type-checks (every `docker build`
  in this repo's CI proves that much), and typically shares code paths with
  something that IS integration-proven, but the path itself has never
  actually run.
- **Not yet tested** — no test of any kind touches it today.

| Capability / path | Status | Notes |
|---|---|---|
| Filesystem backup + restore | **Integration-proven** | `run.sh`: canary file in a named volume, backed up, restored, byte-diffed. |
| Daemon scheduler | **Integration-proven** | `run.sh`: `@every 1m` fires a real second scheduled backup with no CLI involvement. |
| `host_roots` default volume resolution | **Unit-tested + Integration-proven** | Unit: `internal/discovery/volumes_test.go` (default Docker volumes root, and a user `host_roots` entry merging with it rather than replacing it). Integration: every fs-backup itest run (`run.sh`, `run-s3.sh`) resolves a named volume with zero `host_roots` configuration, exactly the "add one label" README claim. |
| HKDF password derivation (`internal/secret/derive.go`) | **Integration-proven, indirectly** | Every itest backup+restore round trip only works if `DeriveRepoPassword` returns the *same* password for the same service name twice (once at write, once at read) — this has succeeded across all three itest suites. There is, however, **no unit test pinning the frozen v1 byte output** (salt, info string, output length, encoding) the file's own doc comment calls a frozen contract; a regression there could silently orphan every repository without any current test catching it before a real restore attempt. Recommended next unit test, not yet written. |
| Label discovery / parsing (`internal/discovery`) | **Unit-tested (partial) + Integration-proven (partial)** | Unit: default host-roots merge, `ballast.notify.*` labels including the `tagwright.backup.*` alias. Integration: `ballast.enable`, `ballast.repo`, `ballast.volumes=none`, and `ballast.stream.<id>.*` end to end (`run-stream.sh`); named-volume mount discovery (`run.sh`, `run-s3.sh`). Not covered by any test: `exclude`/`exclude.<n>`, `retention.*` label parsing, `tags`, the `ballast.*`/`tagwright.backup.*` conflict-rejection rule, and `ballast.name` service-identity override. |
| Stream / DB-dump path (exec → `restic backup --stdin`) | **Integration-proven** | `run-stream.sh`: a real Postgres `pg_dump` piped through docker exec into restic, restored, and diffed against the original schema + canary row. Also proves the `BALLAST_ENABLE_EXEC` gate and `stream=<id>` snapshot tagging. |
| Stream dump failure path | **Integration-proven** | `run-stream.sh`: a bogus `stream.<id>.command` (`false`) makes the backup abort with a non-zero exit and leaves **no snapshot** behind. This was not true before this test suite found it — see the "Bug found and fixed" section below. |
| S3 backend + credential passing | **Integration-proven (via MinIO)** | `run-s3.sh`: a local MinIO stands in for the S3-compatible endpoint; the destination's `env` map resolves through the same secret → child-process-env → restic path a real S3-compatible destination uses, and the backup, snapshot listing, and restore all work against it, with objects confirmed present in the bucket. |
| Real Cloudflare R2 | **Not yet tested — requires operator credentials** | R2 is just an S3-compatible endpoint from restic's point of view; MinIO exercises the identical code path (`internal/engine/restic.go`'s `childEnv`, the `s3:` URL scheme, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env). What MinIO cannot prove: R2-specific network/auth behavior (real TLS endpoint, real R2 account restrictions, R2's actual latency/throughput characteristics). Someone with real R2 credentials should run a manual backup/restore against a throwaway R2 bucket before depending on this in production. |
| Retention / forget | **Integration-proven (execution only)** | `Forget` runs as part of every itest backup with no error. No itest asserts the actual keep/prune *outcome* against a specific policy (e.g., that `keep-daily=2` really leaves exactly 2 daily snapshots after several runs). |
| Prune | **Compile-only** | Thin wrapper over `restic prune`; only reachable via the daemon's `PruneSchedule` (`@weekly` default) or a direct `Engine.Prune` call, neither exercised by any itest. |
| Check | **Compile-only** | Same story as Prune: reachable via `CheckSchedule` (`@monthly` default), never exercised. |
| Notification channels actually firing | **Not yet tested (from Ballast's side)** | Every itest config sets no `notifications`, so beacon's built-in `log` fallback channel fires on every backup — that proves the `Notify`/`Report` call sites work and don't error. Actual delivery logic for `ntfy`, `discord`, `smtp`, `webhook` lives in the `github.com/tagwright/beacon` module, outside this repo, and firing a real external channel needs a live endpoint (a real ntfy topic, Discord webhook, SMTP relay) that doesn't belong in a repo-local automated run. |
| Gatus telemetry sink | **Not yet tested** | Same boundary as notifications: lives in beacon, needs a real Gatus push-URL target. No itest configures `telemetry`. |
| Exec pre/post hooks (`ballast.exec.pre`/`.post`) | **Not yet tested** | Discovery's label parsing for `exec.pre`/`exec.post` (`internal/discovery/fields.go`'s `parseHook`) is structurally identical to the stream-command parsing the stream itest exercises, but the orchestrator's `runHook` — the actual exec, timeout, and non-zero-exit-warns-but-doesn't-abort behavior — has never been run by any test, unit or integration. |
| Stop-for-consistency (`ballast.stop`, `BALLAST_ENABLE_STOP`) | **Not yet tested** | No itest sets `ballast.stop=true` or `BALLAST_ENABLE_STOP=true`; the stop/backup/restart sequence in `internal/orchestrator/backup.go`'s `runBackupSteps` has never actually stopped and restarted a real container under test. |
| Podman adapter | **Compile-only** | `pkg/runtime/podman.go` shares all of `engineClient`'s request/mapping code with the Docker adapter (which IS integration-proven), but the Podman-specific bits — default rootless/rootful socket resolution, the `io.podman.compose.*` label fallback — have never run against an actual Podman socket. |
| Failure paths generally | **Partial** | The stream-dump failure path is integration-proven (see above). Discovery validation errors (label conflicts, `stop`+stream incompatibility, exec/stop used without their global gate) are enforced by code every itest run implicitly relies on not tripping, but no test deliberately triggers and asserts each one. Secret-not-found, wrong repository password, and unreachable-backend failures are untested. |

## Bug found and fixed

Writing the stream-dump failure-path check (`run-stream.sh`'s bogus
`stream.<id>.command` case) found a real bug: a stream dump that exited
non-zero could still leave a written snapshot in the repository, because
`os.Pipe` (what Go's `os/exec` wires a generic `io.Reader` `Stdin` through)
has no way to signal "the writer errored" to the reading end — only a plain
close, indistinguishable from a clean EOF. `restic` would see that as a
normal (if short or empty) end of input, commit a snapshot, and exit 0,
while the *Go-level* stdin-copy error still surfaced as `engine.Restic
.Backup`'s own returned error — discarding the real snapshot ID along with
it, so nothing could clean it up.

Fixed in `internal/engine/restic.go` (`Backup` now parses the summary out of
restic's stdout regardless of whether the invocation itself reported an
error, so a real `SnapshotID` comes back alongside the error when restic
wrote one anyway) and `internal/orchestrator/backup.go` (`runStreamBackup`
uses that to delete the snapshot via the new `engine.Engine.DeleteSnapshot`
before returning the dump's failure). See the commit history for the full
explanation; `run-stream.sh`'s failure-path check now asserts zero snapshots
land after a failed dump, and would catch a regression.

## What's still unproven, bluntly

- **Real R2.** MinIO proves the code path; it does not prove R2 itself. Run
  a manual backup/restore against a throwaway R2 bucket before trusting a
  production R2 destination.
- **Retention math.** Nothing asserts that a `keep-daily`/`keep-weekly`/etc.
  policy actually forgets the snapshots it should and keeps the ones it
  should, only that the `forget` call itself doesn't error.
- **Prune and check.** Never run by any test.
- **Real notification delivery and Gatus telemetry.** Both live in the
  `beacon` module and need live external endpoints to prove; only the
  in-process call sites are exercised here (via the `log` fallback
  channel).
- **Exec pre/post hooks and `ballast.stop`.** Both share plumbing with the
  now-proven stream path (`runtime.Exec` for hooks, `runtime.Stop`/`Start`
  for stop) but neither has been run end to end.
- **Podman.** Entirely untested beyond compiling; only the Docker adapter
  has ever talked to a real socket under test.
- **Most discovery validation errors.** The grammar's reject-and-alert
  rules (label conflicts, incompatible combinations, missing global gates)
  are relied on by every passing itest not tripping them, but none is
  deliberately triggered and asserted.
