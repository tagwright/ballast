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
   failure, or interrupt). Seven scripts today:

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
   - `run-retention.sh` — forces five real backups of a `ballast.retention.
     last=3` labeled service and asserts the *exact* surviving snapshot set
     (not just a count), then runs a real daemon with short
     `BALLAST_PRUNE_SCHEDULE`/`BALLAST_CHECK_SCHEDULE` intervals and confirms
     both `internal/daemon/maintenance.go` actions complete without error,
     then runs `restic check --read-data` directly against the repository.
     See the Retention/Prune/Check section below for exactly what this does
     and does not prove.
   - `run-hooks.sh` — `ballast.exec.pre`/`ballast.exec.post`, gated by
     `BALLAST_ENABLE_EXEC`: both hooks write a distinct marker file into the
     volume being backed up; a `docker exec` after the run confirms both
     actually ran inside the container, and restoring the snapshot proves
     the ordering directly (the pre-marker is present, the post-marker is
     not, because the filesystem backup step runs strictly between the
     two). A second service with a non-zero `exec.pre` proves the run
     aborts with no snapshot written while `exec.post` still runs.
   - `run-stop.sh` — `ballast.stop=true`, gated by `BALLAST_ENABLE_STOP`:
     `docker events` watched across a real backup confirms the container
     was actually stopped (`die`) and started again (`start`), in that
     order, a fresh `State.StartedAt` confirms the restart, and the
     snapshot taken while stopped restores byte-for-byte. Also confirms
     discovery rejects `stop=true` combined with a stream backup.
   - `run-notify.sh` — a real `binwiederhier/ntfy` server and a
     `notifications: [{type: ntfy, ...}]` config, proving config →
     `daemon.BuildNotifier` → `orchestrator.reportOutcome` → beacon → an
     actual message landing in the topic (title, body, and ntfy priority
     all checked, via ntfy's own JSON poll API), that
     `ballast.notify.suppress=true` produces no message, and that
     `ballast.notify.on-success=true` escalates a successful backup's
     message to Warning-level (ntfy priority 4).

   See `test/integration/README.md` for how to run each one.

3. **Deliberately manual.** A few things are exercised by hand rather than
   by an automated harness, noted individually in the matrix below — mostly
   because they need a real external account (Cloudflare R2) or a real
   notification backend's live endpoint that isn't self-hostable the way
   ntfy is (an actual Discord webhook, SMTP relay, or Gatus instance),
   neither of which belongs in a repo-local test run.

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
| HKDF password derivation (`internal/secret/derive.go`) | **Unit-tested (golden value) + Integration-proven (indirectly)** | Unit: `internal/secret/derive_test.go`'s `TestDeriveRepoPasswordGoldenValues` pins `DeriveRepoPassword`'s output for a fixed master and three service names against exact base64 strings computed once with this code and hardcoded as constants — this is the tripwire the doc comment's "frozen v1 contract" warning asked for: a regression in the salt, info template, output length, or encoding fails this test immediately instead of silently orphaning every repository. `TestDeriveRepoPasswordDeterministic` and `TestDeriveRepoPasswordDistinctPerService` cover the two properties `ballast key <service>` depends on (same input always reproduces the same password; different service names never collide). `TestLoadMasterRejectsShortMaster` / `TestLoadMasterAcceptsMinimumLength` pin the `minMasterKeyBytes` (32) boundary on both sides. Integration: every itest backup+restore round trip only works if the same derivation reproduces the same password at write and read time, which has succeeded across all four itest suites. |
| Label discovery / parsing (`internal/discovery`) | **Unit-tested (partial) + Integration-proven (partial)** | Unit: default host-roots merge, `ballast.notify.*` labels including the `tagwright.backup.*` alias. Integration: `ballast.enable`, `ballast.repo`, `ballast.volumes=none`, and `ballast.stream.<id>.*` end to end (`run-stream.sh`); named-volume mount discovery (`run.sh`, `run-s3.sh`). Not covered by any test: `exclude`/`exclude.<n>`, `retention.*` label parsing, `tags`, the `ballast.*`/`tagwright.backup.*` conflict-rejection rule, and `ballast.name` service-identity override. |
| Stream / DB-dump path (exec → `restic backup --stdin`) | **Integration-proven** | `run-stream.sh`: a real Postgres `pg_dump` piped through docker exec into restic, restored, and diffed against the original schema + canary row. Also proves the `BALLAST_ENABLE_EXEC` gate and `stream=<id>` snapshot tagging. |
| Stream dump failure path | **Integration-proven** | `run-stream.sh`: a bogus `stream.<id>.command` (`false`) makes the backup abort with a non-zero exit and leaves **no snapshot** behind. This was not true before this test suite found it — see the "Bug found and fixed" section below. |
| S3 backend + credential passing | **Integration-proven (via MinIO)** | `run-s3.sh`: a local MinIO stands in for the S3-compatible endpoint; the destination's `env` map resolves through the same secret → child-process-env → restic path a real S3-compatible destination uses, and the backup, snapshot listing, and restore all work against it, with objects confirmed present in the bucket. |
| Real Cloudflare R2 | **Not yet tested — requires operator credentials** | R2 is just an S3-compatible endpoint from restic's point of view; MinIO exercises the identical code path (`internal/engine/restic.go`'s `childEnv`, the `s3:` URL scheme, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env). What MinIO cannot prove: R2-specific network/auth behavior (real TLS endpoint, real R2 account restrictions, R2's actual latency/throughput characteristics). Someone with real R2 credentials should run a manual backup/restore against a throwaway R2 bucket before depending on this in production. |
| Retention / forget, count-based (`keep-last`) | **Integration-proven, outcome asserted** | `run-retention.sh`: a service labeled `ballast.retention.last=3` is backed up five times (each real backup also runs `Forget` with that policy, exactly as production does — see `runBackupSteps`). The script records the snapshot ID each run produces and asserts the final surviving set is *exactly* the 3 newest (iterations 3, 4, 5), i.e. that `keep-last` forgets the right snapshots, not merely that `forget` exits 0. |
| Retention / forget, time-based (`keep-daily`/`keep-weekly`/`keep-monthly`/`keep-yearly`/`keep-within`) | **Not asserted** | Deliberately not exercised: proving these deterministically needs snapshots with controlled, spread-out timestamps (real days/weeks apart, or a faked clock), which this harness does not attempt. `keep-last` was chosen instead specifically because it is assertable with real, back-to-back runs. The code path is identical (`internal/engine/restic.go`'s `Forget` builds one `restic forget` invocation from whichever `--keep-*` flags the policy sets), so this is a real, not merely theoretical, gap — restic's own retention math for the time-based keeps is untested by Ballast itself. |
| Prune | **Integration-proven** | `run-retention.sh`: after the keep-last forget above has actually removed data, a real daemon with a short `BALLAST_PRUNE_SCHEDULE` runs prune (`internal/daemon/maintenance.go`'s scheduled path, the same one production uses) and the script confirms from the daemon's own logs that it completed with no error, then re-lists snapshots to confirm the repository is still valid (all 3 survivors still present) afterward. |
| Check | **Integration-proven, including `--read-data`** | `run-retention.sh`: the daemon's `BALLAST_CHECK_SCHEDULE` path runs `Check(ctx, repo, false)` (matching `maintenance.go`, which hardcodes `readData=false` for the scheduled path) and the script confirms a clean completion from the daemon logs. Separately, since no CLI command or schedule ever passes `readData=true`, the script also shells out directly to the same `restic` binary bundled in `ballast:itest` and runs `restic check --read-data` against the same repository (using the password `ballast key` derives), confirming "no errors were found" against real pack data. |
| Notification channels actually firing | **Integration-proven (ntfy, end to end through Ballast's own orchestrator) + not yet tested (discord/smtp/webhook)** | `run-notify.sh`: a real `binwiederhier/ntfy` server, a `notifications` channel pointed at it in `notify.itest.yml`, and three real `ballast backup` runs. This is the one itest that exercises the *whole* path other suites don't: `config.ChannelConfig` → `daemon.BuildNotifier` → `orchestrator.reportOutcome` → `beacon.Beacon.Notify` → a real HTTP POST → a message actually readable back from ntfy's own JSON poll API, with title, body, and priority all asserted. Also proves the per-service controls: `ballast.notify.suppress=true` produces no message at all, and `ballast.notify.on-success=true` raises a successful backup's message from ntfy priority 3 (default/`LevelInfo`) to 4 (high/`LevelWarning`). Every other itest still configures no `notifications` at all, so beacon's built-in `log` fallback fires there instead; `discord`, `smtp`, and `webhook` backends remain untested from Ballast's side (they live in the `github.com/tagwright/beacon` module and need a live external endpoint or account this harness doesn't have). |
| Gatus telemetry sink | **Not yet tested** | Same boundary as the other notification backends: lives in beacon, needs a real Gatus push-URL target. No itest configures `telemetry`. |
| Exec pre/post hooks (`ballast.exec.pre`/`.post`) | **Integration-proven (pre-hook path) + partially unproven (post-hook failure path)** | `run-hooks.sh`: a service with both `exec.pre` and `exec.post` labels, each writing a distinct marker file into the volume being backed up. `docker exec` after the run confirms both hooks actually executed inside the container; restoring the resulting snapshot proves the ordering directly (the pre-marker is present in the snapshot, the post-marker is not, because `RunBackup`'s filesystem-backup step runs strictly between the two hooks), which is a stronger proof than a timestamp comparison would be. A second service with a non-zero `exec.pre` confirms the run aborts with **no snapshot written**, and that `exec.post` still runs regardless (`docker exec` confirms its own marker). **Not proven**: `orchestrator.RunBackup`'s doc-commented claim that a non-zero `exec.post` "only warns" rather than aborting or failing the command — no itest gives `exec.post` a failing command, only a succeeding one. `exec.pre`/`exec.post`'s `.timeout`/`.user` sub-labels are also unexercised. |
| Stop-for-consistency (`ballast.stop`, `BALLAST_ENABLE_STOP`) | **Integration-proven** | `run-stop.sh`: a `ballast.stop=true` labeled service. `docker events` watched across a real `ballast backup` confirms the container actually received a `die` event (stopped) followed by a `start` event (restarted), in that order; `docker inspect`'s `State.StartedAt` confirms a genuinely fresh start, not merely "still running"; and the snapshot the backup wrote restores byte-for-byte. Also confirms discovery's grammar rejects `stop=true` combined with a stream backup as incompatible (found a real bug doing this — see "Bugs found and fixed" below). **Not proven**: a direct concurrent-write race (i.e. that data really can't change mid-backup while stopped) — the test container has no writer process to race against, so this is still resting on `runBackupSteps`' code structure (fs backup runs strictly inside the stop/defer-start closure) rather than an empirical race demonstration. Also unproven: the `defaultStopTimeoutSeconds` (30s) SIGKILL-after-timeout fallback path itself — the test container responds to `SIGTERM` immediately (`--init` + `exec sleep`), so only the graceful-stop path has actually run. |
| Podman adapter | **Compile-only** | `pkg/runtime/podman.go` shares all of `engineClient`'s request/mapping code with the Docker adapter (which IS integration-proven), but the Podman-specific bits — default rootless/rootful socket resolution, the `io.podman.compose.*` label fallback — have never run against an actual Podman socket. |
| Failure paths generally | **Partial** | The stream-dump failure path, the `exec.pre` failure path, and the `stop`+stream discovery-rejection path are all integration-proven (see above). Other discovery validation errors (label conflicts, exec/stop used without their global gate) are enforced by code every itest run implicitly relies on not tripping, but no test deliberately triggers and asserts them. Secret-not-found, wrong repository password, and unreachable-backend failures are untested. |

## Bugs found and fixed

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

Writing `run-retention.sh`'s final `restic check --read-data` step found a
second thing worth recording, though it was a bug in the *harness script*,
not in Ballast itself: the first draft mounted the repository directory
read-only (`:ro`) for that direct `restic` invocation, on the reasoning that
`check` never writes snapshot or pack data. `restic check` still takes an
exclusive repository lock for its duration, which requires writing a lock
file under `<repo>/locks/`; against a read-only mount that write fails and
restic retries forever with growing backoff, hanging indefinitely instead of
producing a clean error. Fixed by dropping `:ro` from that one mount in
`test/integration/run-retention.sh`. Worth remembering operationally too:
nothing in Ballast's own engine or daemon code assumes a writable repo mount
for `check` (`internal/engine/restic.go`'s `Check` issues a plain `restic
check [--read-data]`), so a real deployment that mounts a repository
read-only for defense-in-depth would hit the identical hang.

Writing `run-stop.sh`'s stop+stream incompatibility check found a third bug,
this time a real one in Ballast itself, not a harness artifact: setting both
`ballast.stop=true` and a `ballast.stream.<id>.*` label on a container is
supposed to be rejected at discovery (`internal/discovery/discovery.go`'s
`validate` correctly returns an "incompatible" error for it), but `ballast
backup <service>` reported a generic "service ... not found" instead of that
error. `discovery.Discover`'s own doc comment promises a validation error is
"surfaced to the caller so it can alert", and every CLI call site that looks
up a single service by name (`internal/cli/backup.go`'s service loop,
`internal/cli/deps.go`'s `discoverService`, used by `snapshots`/`restore`'s
disaster-recovery fallback) matches a container by `BackupSpec.Service`
*before* checking the error, expecting `Discover` to hand back a non-nil
spec alongside a non-nil `validate()` error. `Discover`'s final branch
returned a `nil` spec instead, so those call sites' `if s == nil { continue
}` silently skipped straight past the very container being looked up, and
the real validation error (label conflicts, `stop`+stream, exec/stop used
without its gate — anything `validate()` rejects) was discarded in favor of
a misleading "not found". The daemon's own discovery pass
(`internal/daemon/watch.go`'s `discoverOne`) never hit this, since it only
checks the error and never needs the spec on that path — which is exactly
why it shipped unnoticed. Fixed in `internal/discovery/discovery.go` by
returning `spec` (not `nil`) alongside the `validate()` error.

## What's still unproven, bluntly

- **Real R2.** MinIO proves the code path; it does not prove R2 itself. Run
  a manual backup/restore against a throwaway R2 bucket before trusting a
  production R2 destination.
- **Time-based retention** (`keep-daily`/`keep-weekly`/`keep-monthly`/
  `keep-yearly`/`keep-within`). `keep-last` is now proven with an exact
  surviving-set assertion (`run-retention.sh`), but the time-based keeps
  need snapshots spread across real (or faked) time to assert
  deterministically, which no itest attempts. This is restic's own
  retention math, driven by whichever `--keep-*` flags
  `internal/engine/restic.go`'s `Forget` sets from the policy, so it is a
  real gap in what Ballast itself has verified, not just an academic one.
- **Discord, SMTP, webhook notification delivery, and Gatus telemetry.**
  ntfy delivery is now integration-proven end to end through Ballast's own
  orchestrator (`run-notify.sh`); the other three beacon notification
  backends and the Gatus telemetry sink still need a live external endpoint
  or account this repo-local harness doesn't have.
- **`exec.post`'s "failure only warns" behavior.** `run-hooks.sh` proves
  `exec.pre` failing aborts the run (no snapshot, `exec.post` still runs),
  but no itest gives `exec.post` itself a *failing* command, so
  `orchestrator.RunBackup`'s doc-commented claim that a non-zero
  `exec.post` only warns (rather than failing the command or the outcome
  report) rests on reading the code, not on an assertion. `exec.pre`/
  `exec.post`'s `.timeout` and `.user` sub-labels are also unexercised.
- **`ballast.stop`'s concurrent-write race and SIGKILL-after-timeout
  path.** `run-stop.sh` proves the container is genuinely stopped (a real
  `docker events` `die`) and restarted (`start`, a fresh `State.StartedAt`)
  around the backup, and that the resulting snapshot restores correctly.
  It does not (and structurally cannot, without a real writer process to
  race) prove data can't change mid-backup while stopped — that still
  rests on `runBackupSteps`' code structure, not an empirical race. The
  test container also responds to `SIGTERM` immediately, so the
  `defaultStopTimeoutSeconds` (30s) SIGKILL fallback path has never
  actually fired under test.
- **Podman.** Entirely untested beyond compiling; only the Docker adapter
  has ever talked to a real socket under test.
- **Most discovery validation errors.** `run-stop.sh` now deliberately
  triggers and asserts one (`stop`+stream incompatibility), which is also
  what found the `Discover` nil-spec bug above. The rest of the grammar's
  reject-and-alert rules (label conflicts, exec/stop used without their
  global gate) are relied on by every passing itest not tripping them, but
  none is deliberately triggered and asserted.
