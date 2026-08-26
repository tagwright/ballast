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
   failure, or interrupt). Thirteen scripts today:

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
   - `run-watch.sh` — the daemon's live socket-event watch path
     (`internal/daemon/watch.go`'s `watchLoop`), the one path every prior
     script left unproven since they all only exercise startup discovery
     (`discoverAll`, run once before the watch loop even starts). A real
     `ballast daemon` starts before a labeled container exists; the
     container is created while the daemon runs, and a real Docker "start"
     event drives discovery and a real backup (a snapshot lands in the
     repository). `docker rm -f` then drives a real die+destroy event pair,
     confirmed by the `daemon: service unregistered` log line this pass
     added (see "Bugs found and fixed" below), and a full schedule interval
     afterward confirms no further backup fires.
   - `run-splay.sh` — three services on the daemon's global schedule,
     proving `Concurrency=1` (the grammar's default) actually serializes
     overlapping backups rather than merely being documented to: each
     service's `exec.pre` sleeps 5 seconds and writes a whole-second start
     marker into a shared bind-mounted host directory, `exec.post` writes
     the end marker, and the script asserts no two services'
     `[start, end]` intervals ever overlap. The splay-slot-distinctness half
     of the claim (that a fleet on the same period alias lands on different
     slots) is proven separately at the unit level, since a real `@daily`
     wait is impractical for a live itest.
   - `run-volumes.sh` — multi-volume backup and narrowing
     (`internal/discovery/volumes.go`), never exercised end to end before
     (only `ballast.volumes=none` had been, via `run-stream.sh`): a service
     with two named volumes backs up both; `ballast.volumes=<name>` narrows
     to one, confirmed absent-vs-present in the restore;
     `ballast.volumes.exclude=<name>` proves the inverse; and a fourth
     service proves `ballast.exclude=<glob>` drops matching files and
     `ballast.exclude-caches=true` drops a real `CACHEDIR.TAG`-tagged
     directory's contents (per restic's documented behavior, the tag file
     and the now-empty directory itself survive) from the restored
     snapshot.
   - `run-dupe.sh` — two containers resolving to the same service name via
     `ballast.name`, proving `internal/daemon/registry.go`'s duplicate-
     service-name rejection live: the daemon logs the rejection of the
     second container, keeps running (no crash), and the latest snapshot
     after another full schedule round still only ever contains the first
     container's data, not a silent double-backup.
   - `run-alias.sh` — a service labeled entirely under `tagwright.backup.*`
     (no `ballast.*` label at all): a real backup, a snapshot listing
     confirming `tagwright.backup.tags` reached the snapshot, and a restore,
     proving the org-namespaced alias works identically to `ballast.*` end
     to end, not just at the unit level.
   - `run-conflict.sh` — a container labeling the same suffix differently
     under both prefixes (`ballast.repo=A` vs `tagwright.backup.repo=B`):
     rejected by the daemon's discovery pass (logged, never backed up, no
     crash) and by `ballast backup <service>`, which (after this pass's fix,
     see "Bugs found and fixed" below) surfaces the real conflict instead of
     a misleading "not found".

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
| Daemon socket-event watch (add on container start, drop on die/destroy) | **Integration-proven** | `run-watch.sh`: a real Docker "start" event for a container the daemon has never seen drives discovery and a real backup; `docker rm -f`'s die+destroy pair drops the scheduled job, confirmed by the `daemon: service unregistered` log line (added this pass, see "Bugs found and fixed"), and a full schedule interval afterward produces no further backup. Every prior itest only proved startup discovery (`discoverAll`), never the watch loop. |
| Multiple services, same schedule alias: `Concurrency=1` serialization | **Integration-proven** | `run-splay.sh`: three services with an artificial `exec.pre` delay, asserting from real whole-second start/end markers that no two services' backup runs ever overlap. |
| Splay-slot distinctness across a fleet (same period alias, different names) | **Unit-tested** | `internal/schedule/schedule_test.go`'s `TestDailySplayDistinctAcrossThreeServices` (three real service names, pairwise-distinct `@daily` slots), extending the existing pairwise `@hourly` test to a small fleet. A real `@daily` wait is impractical for a live itest. |
| `host_roots` default volume resolution | **Unit-tested + Integration-proven** | Unit: `internal/discovery/volumes_test.go` (default Docker volumes root, and a user `host_roots` entry merging with it rather than replacing it). Integration: every fs-backup itest run (`run.sh`, `run-s3.sh`) resolves a named volume with zero `host_roots` configuration, exactly the "add one label" README claim. |
| HKDF password derivation (`internal/secret/derive.go`) | **Unit-tested (golden value) + Integration-proven (indirectly)** | Unit: `internal/secret/derive_test.go`'s `TestDeriveRepoPasswordGoldenValues` pins `DeriveRepoPassword`'s output for a fixed master and three service names against exact base64 strings computed once with this code and hardcoded as constants — this is the tripwire the doc comment's "frozen v1 contract" warning asked for: a regression in the salt, info template, output length, or encoding fails this test immediately instead of silently orphaning every repository. `TestDeriveRepoPasswordDeterministic` and `TestDeriveRepoPasswordDistinctPerService` cover the two properties `ballast key <service>` depends on (same input always reproduces the same password; different service names never collide). `TestLoadMasterRejectsShortMaster` / `TestLoadMasterAcceptsMinimumLength` pin the `minMasterKeyBytes` (32) boundary on both sides. Integration: every itest backup+restore round trip only works if the same derivation reproduces the same password at write and read time, which has succeeded across all four itest suites. |
| Label discovery / parsing (`internal/discovery`) | **Unit-tested + Integration-proven** | Unit: default host-roots merge, `ballast.notify.*` labels including the `tagwright.backup.*` alias, the prefix-conflict error carrying a usable spec (`TestDiscoverPrefixConflictReturnsSpecAlongsideError`), and the global `exclude` list merging with a service's own (`TestDiscoverGlobalExcludeMergesWithLabel`). Integration: `ballast.enable`, `ballast.repo`, `ballast.volumes=none`, and `ballast.stream.<id>.*` end to end (`run-stream.sh`); named-volume mount discovery (`run.sh`, `run-s3.sh`); `ballast.volumes`/`ballast.volumes.exclude` narrowing, `ballast.exclude`, and `ballast.exclude-caches` (`run-volumes.sh`); `ballast.name` service-identity override and the duplicate-service-name rejection rule (`run-dupe.sh`); the `tagwright.backup.*` alias end to end including `tags` and `retention.last` (`run-alias.sh`); and the `ballast.*`/`tagwright.backup.*` conflict-rejection rule via both the daemon and the CLI (`run-conflict.sh`). Still not covered by any test: individual `retention.hourly`/`.daily`/`.weekly`/`.monthly`/`.yearly`/`.within`/`.keep-tags` label parsing beyond `retention.last`, and the indexed `exclude.<n>` escape hatch (only the CSV `exclude` form has run). |
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
| Failure paths generally | **Partial** | The stream-dump failure path, the `exec.pre` failure path, the `stop`+stream discovery-rejection path, the `ballast.*`/`tagwright.backup.*` prefix-conflict rejection (`run-conflict.sh`), and the duplicate-service-name rejection (`run-dupe.sh`) are all integration-proven (see above). `exec`/`stop` used without their global gate is enforced by code every non-exec/non-stop itest run implicitly relies on not tripping, but no test deliberately triggers and asserts it. Secret-not-found, wrong repository password, and unreachable-backend failures are untested. |

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

Auditing `internal/discovery` and `internal/daemon` for this pass's breadth
tests, before even running anything, found the exact same class of bug a
second time at a different point inside `Discover`, plus two more real
bugs, none caught by any prior itest because none of them had ever labeled
a container to trigger the conditions:

**Fourth bug**: `normalizeLabels`' own conflict error (the same suffix set
to different values under `ballast.*` and `tagwright.backup.*`) made
`Discover` return a `nil` spec too, the identical failure mode the third bug
above already fixed for `validate()`'s errors — just one call earlier, before
a spec is even built. `run-conflict.sh` (written to prove the conflict is
rejected at all) caught it immediately: `ballast backup <service>` reported
"not found" instead of the real conflict. Fixed in
`internal/discovery/discovery.go` the same way as the third bug: `Discover`
now resolves a best-effort `Service` identity from what doesn't depend on
`norm` (the compose-service and container-name fallbacks in
`resolveServiceName` both work against a `nil` map) and returns it alongside
the error.

**Fifth bug**: `config.Config.Exclude` — documented on the struct itself as
"the global glob-exclude list, additive to any per-service `ballast.exclude`
labels", parsed from `BALLAST_EXCLUDE` or `ballast.yml`, with its own env
overlay code and default-handling — was never actually merged into any
service's `BackupSpec.Excludes` anywhere in the codebase. A fully wired,
fully documented setting that silently did nothing; nothing in the itest
suite ever set it, so nothing ever noticed. Found while auditing
`internal/discovery/discovery.go` for the `run-volumes.sh` excludes tests,
not by a failing test (this one wasn't itest-covered before the fix either —
see `TestDiscoverGlobalExcludeMergesWithLabel`). Fixed by merging
`cfg.Exclude` with the service's own `ballast.exclude`/`ballast.exclude.<n>`
in `Discover`.

**Sixth bug**: a die/destroy lifecycle event silently unregistered a
service's scheduled job — `internal/daemon/registry.go`'s
`unregisterContainer` removed it from both of `registry`'s maps and
cancelled the scheduler entry, but logged nothing at all. `register` already
logs on the way in, both the normal case (implicitly, via the scheduler) and
the duplicate-rejection case (explicitly); the corresponding way out had no
observable trace, so an operator watching the daemon's logs after a
container disappeared would see nothing until the next scheduled fire
silently never happened. Found while designing `run-watch.sh`'s removal
assertion, which needed exactly this signal to know *when* the
unregistration had actually taken effect rather than guessing from a fixed
sleep. Fixed by having `unregisterContainer` return the service name it
actually removed, and `watch.go`'s `handleEvent` logging
`"daemon: service unregistered"` when it isn't empty.

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
- **Exec/stop used without their global gate, and secret/backend
  failures.** `run-stop.sh`'s `stop`+stream incompatibility and
  `run-conflict.sh`'s prefix conflict are now both deliberately triggered
  and asserted (the latter is also what found the fourth bug above). Still
  untriggered: a `ballast.stop`/`ballast.exec.*`/`ballast.stream.*` label
  used with `BALLAST_ENABLE_STOP`/`BALLAST_ENABLE_EXEC` left at its
  default-false. Secret-not-found, wrong repository password, and
  unreachable-backend failures also remain untested.
- **Individual retention label parsing beyond `retention.last`.**
  `run-retention.sh` proves `keep-last` end to end, and `run-alias.sh`
  additionally proves `retention.last` parses through the
  `tagwright.backup.*` alias, but `retention.hourly`/`.daily`/`.weekly`/
  `.monthly`/`.yearly`/`.within`/`.keep-tags` label parsing has no test of
  its own (time-based retention's *outcome* is separately unproven for the
  reason above).
- **The indexed `ballast.exclude.<n>` escape hatch.** `run-volumes.sh`
  proves the CSV `ballast.exclude=<glob>` form end to end; the mutually
  exclusive indexed form (and the "setting both is a validation error" rule)
  has no itest of its own.
- **Splay's on/off toggle.** `config.Config.Splay` is parsed from
  `BALLAST_SPLAY` (and documented as turning the deterministic per-service
  splay of period aliases on or off) but is never actually read by
  `internal/daemon` or `internal/schedule` — the four period aliases are
  always splayed regardless of this setting. Found during this pass's
  audit but deliberately left as a documented gap rather than fixed: unlike
  the exclude-merge bug, `Splay`'s zero value (`false`) would, if wired up
  naively, *disable* the anti-stampede splay by default, which contradicts
  every other doc comment's description of the feature — that mismatch
  needs a real design decision (what should the default be, and does
  disabling splay mean "always fire at the canonical boundary" or something
  else), not a mechanical wiring fix assumed under a testing pass.
