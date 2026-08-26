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
   failure, or interrupt). Seventeen scripts today:

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
   - `run-password-secret.sh` — `ballast.password-secret=<name>`: a real
     backup + restore round-trip through the named secret, plus the actual
     proof (not just the round-trip, which would also pass if the override
     silently did nothing): shelling out directly to the same restic binary
     Ballast bundles, the master-key-derived password (`ballast key`) is
     confirmed to **not** open the repository, while the named secret's own
     value does.
   - `run-repo-path.sh` — `ballast.repo.path=<subpath>`: a real backup
     lands at the overridden sub-path on the host-visible repos directory
     (a real restic `config` object found there), and explicitly does
     **not** exist at the default, un-overridden service-name path;
     restore then round-trips through the same override.
   - `run-sftp.sh` — the restic SFTP backend against a throwaway
     `atmoz/sftp` server on a `ballast-itest-net` user network: a real
     `sftp:user@host:path` destination, key-based auth via a fresh
     itest-only keypair, a real backup confirmed to have landed on the
     SFTP server's own filesystem (independent of Ballast's view), and a
     restore that byte-matches the canary. Found a real production gap
     doing this — see "Bugs found and fixed" below.
   - `run-retention-time.sh` — not a container itest like the sixteen
     above: it runs `internal/engine`'s `TestForgetKeepDailyTimeBased`
     (build-tag `integration`) inside a throwaway `golang:1.25` + `restic`
     container, seeding a repository with snapshots at controlled synthetic
     times via `restic backup --time` and exercising the real
     `engine.Restic.Forget` code path with `RetentionPolicy{Daily: 3}`. See
     the "Retention / forget, time-based" row below for exactly what this
     proves and does not.

   See `test/integration/README.md` for how to run each one.

3. **Deliberately manual.** A few things are exercised by hand rather than
   by an automated harness, noted individually in the matrix below — mostly
   because they need a real external account (Cloudflare R2) or a real
   notification backend's live endpoint that isn't self-hostable the way
   ntfy is (an actual Discord webhook, SMTP relay, or Gatus instance),
   neither of which belongs in a repo-local test run.

## Inert-field audit

A "documented but inert" bug (config.Config.Exclude never merged into a
service's excludes; config.Config.Splay never read by the scheduler --
both found and the latter fixed this pass, see below) is worse than a
missing feature: it looks configured, ships in a `ballast.yml`, and does
nothing, with no error to notice. This pass traced EVERY field on
`config.Config` and EVERY label suffix `internal/discovery` accepts to the
line of code that actually consults it, not just the line that parses it.

**Every `config.Config` field:**

| Field | Status | Consulted at |
|---|---|---|
| `Destinations` | WIRED | `orchestrator.BuildRepo` |
| `DefaultDestination` | WIRED | `discovery.Discover` |
| `Schedule` | WIRED | `daemon/registry.go`'s `register` (per-service fallback) |
| `Window` | WIRED | `daemon.schedulerConfig` -> `schedule.Scheduler`'s splay window |
| `Splay` | **was INERT, fixed this pass** | `daemon.schedulerConfig` -> `schedule.Scheduler.splay` -> `schedule.Parse` |
| `Retention` | WIRED | `orchestrator/retention.go`'s `defaultRetentionPolicy` (global fallback) |
| `Exclude` | WIRED (fixed a prior pass) | `discovery.Discover`'s `mergeExcludes` |
| `DiscoverExclude` | WIRED | `discovery/volumes.go`'s `isEligibleMount` |
| `HostRoots` | WIRED | `discovery/volumes.go`'s `translateHostPath` |
| `SecretsDir` | WIRED | `secret.FileEnvResolver`, built in both `daemon.Run` and the CLI's `buildCommonDeps`/`key` command |
| `EnableExec` | WIRED | `discovery.validate` (gates `stream.*`/`exec.*`) |
| `EnableStop` | WIRED | `discovery.validate` (gates `stop`) |
| `PruneSchedule` | WIRED | `daemon/maintenance.go`'s `scheduleMaintenance` |
| `CheckSchedule` | WIRED | `daemon/maintenance.go`'s `scheduleMaintenance` |
| `Concurrency` | WIRED | `daemon.schedulerConfig` -> `schedule.Scheduler`'s worker pool |
| `Notifications` | WIRED | `daemon.BuildNotifier` |
| `Telemetry` | WIRED | `daemon.BuildNotifier` |
| `Runtime` | WIRED | `daemon.buildRuntime` / the CLI's own `buildRuntime` |
| `Socket` | WIRED | `daemon.dockerSocket`/`podmanSocket` / the CLI's own copies |

Every field is now WIRED. `Splay` was the only remaining inert one this
pass found; its intended wiring was mechanical (feed it into
`schedule.Parse` as the switch that already exists between "splay this
alias" and "parse it literally"), so it was fixed rather than left open --
see "Splay, fixed" under "Bugs found and fixed" below.

**Every label suffix `internal/discovery` accepts** (`enable`, `name`,
`repo`, `repo.path`, `password-secret`, `volumes`, `volumes.exclude`,
`exclude`, `exclude.<n>`, `exclude-caches`, `stream.<id>.*`, `exec.*`,
`stop`, `schedule`, `retention.*`, `tags`, `notify.*`) traces through to a
field on `discovery.BackupSpec` that `internal/orchestrator` (or
`internal/daemon/registry.go`, for `schedule`) actually consults, with no
exceptions found: **every label is WIRED.** This is not a surprise this
pass discovered fresh -- it is the state prior passes already got to by
fixing the third, fourth, and fifth bugs below (the two nil-spec bugs and
the global-exclude-merge bug), all of which were exactly this class of
problem at the label-parsing layer. This pass's job was confirming that
state still holds after the fact, field by field, not finding new label
bugs -- and it does hold: no label-level regressions, and no new inert
label found.

No field or label was left inert pending a design decision this pass: the
one inert field found (`Splay`) had an obvious, mechanical fix and was
fixed. If a future pass finds one that genuinely needs a design call
(ambiguous default, unclear interaction with another field), it belongs
here, called out explicitly, the same way `Splay` was called out and left
alone in the PRIOR pass before this one resolved it.

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
| `Splay`'s on/off toggle (`BALLAST_SPLAY`) | **Unit-tested** | Was inert (parsed, never read) before this pass -- see "Splay, fixed" under "Bugs found and fixed" below. Now: `internal/config/config_test.go` proves `Load` defaults `Splay` to true (a nil `*bool`, not the bool zero value) and honors an explicit `BALLAST_SPLAY=false`/`splay: false`; `internal/schedule/scheduler_test.go`'s `TestNewDefaultsSplayOnWhenNil`/`TestNewHonorsExplicitSplayFalse` prove `Scheduler` actually reads it; `internal/schedule/schedule_test.go`'s `TestSplayFalse*` prove `Parse(..., splay=false)` lands `@daily`/`@hourly` on the canonical, unsplayed boundary instead of a job-name-derived slot. No live itest: the distinction only matters for a real `@daily`/`@hourly` wait, which the existing splay-slot-distinctness gap above already rules out as impractical here. |
| `host_roots` default volume resolution | **Unit-tested + Integration-proven** | Unit: `internal/discovery/volumes_test.go` (default Docker volumes root, and a user `host_roots` entry merging with it rather than replacing it). Integration: every fs-backup itest run (`run.sh`, `run-s3.sh`) resolves a named volume with zero `host_roots` configuration, exactly the "add one label" README claim. |
| HKDF password derivation (`internal/secret/derive.go`) | **Unit-tested (golden value) + Integration-proven (indirectly)** | Unit: `internal/secret/derive_test.go`'s `TestDeriveRepoPasswordGoldenValues` pins `DeriveRepoPassword`'s output for a fixed master and three service names against exact base64 strings computed once with this code and hardcoded as constants — this is the tripwire the doc comment's "frozen v1 contract" warning asked for: a regression in the salt, info template, output length, or encoding fails this test immediately instead of silently orphaning every repository. `TestDeriveRepoPasswordDeterministic` and `TestDeriveRepoPasswordDistinctPerService` cover the two properties `ballast key <service>` depends on (same input always reproduces the same password; different service names never collide). `TestLoadMasterRejectsShortMaster` / `TestLoadMasterAcceptsMinimumLength` pin the `minMasterKeyBytes` (32) boundary on both sides. Integration: every itest backup+restore round trip only works if the same derivation reproduces the same password at write and read time, which has succeeded across all four itest suites. |
| Label discovery / parsing (`internal/discovery`) | **Unit-tested + Integration-proven** | Unit: default host-roots merge, `ballast.notify.*` labels including the `tagwright.backup.*` alias, the prefix-conflict error carrying a usable spec (`TestDiscoverPrefixConflictReturnsSpecAlongsideError`), and the global `exclude` list merging with a service's own (`TestDiscoverGlobalExcludeMergesWithLabel`). Integration: `ballast.enable`, `ballast.repo`, `ballast.volumes=none`, and `ballast.stream.<id>.*` end to end (`run-stream.sh`); named-volume mount discovery (`run.sh`, `run-s3.sh`); `ballast.volumes`/`ballast.volumes.exclude` narrowing, `ballast.exclude`, and `ballast.exclude-caches` (`run-volumes.sh`); `ballast.name` service-identity override and the duplicate-service-name rejection rule (`run-dupe.sh`); the `tagwright.backup.*` alias end to end including `tags` and `retention.last` (`run-alias.sh`); the `ballast.*`/`tagwright.backup.*` conflict-rejection rule via both the daemon and the CLI (`run-conflict.sh`); `ballast.password-secret` (`run-password-secret.sh`); and `ballast.repo.path` (`run-repo-path.sh`). Still not covered by any test: individual `retention.hourly`/`.weekly`/`.monthly`/`.yearly`/`.within`/`.keep-tags` label parsing beyond `retention.last` (`retention.daily`'s *outcome* is proven, but not from a label -- see the retention rows above), and the indexed `exclude.<n>` escape hatch (only the CSV `exclude` form has run). |
| Stream / DB-dump path (exec → `restic backup --stdin`) | **Integration-proven** | `run-stream.sh`: a real Postgres `pg_dump` piped through docker exec into restic, restored, and diffed against the original schema + canary row. Also proves the `BALLAST_ENABLE_EXEC` gate and `stream=<id>` snapshot tagging. |
| Stream dump failure path | **Integration-proven** | `run-stream.sh`: a bogus `stream.<id>.command` (`false`) makes the backup abort with a non-zero exit and leaves **no snapshot** behind. This was not true before this test suite found it — see the "Bug found and fixed" section below. |
| S3 backend + credential passing | **Integration-proven (via MinIO)** | `run-s3.sh`: a local MinIO stands in for the S3-compatible endpoint; the destination's `env` map resolves through the same secret → child-process-env → restic path a real S3-compatible destination uses, and the backup, snapshot listing, and restore all work against it, with objects confirmed present in the bucket. |
| Real Cloudflare R2 | **Not yet tested — requires operator credentials** | R2 is just an S3-compatible endpoint from restic's point of view; MinIO exercises the identical code path (`internal/engine/restic.go`'s `childEnv`, the `s3:` URL scheme, `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env). What MinIO cannot prove: R2-specific network/auth behavior (real TLS endpoint, real R2 account restrictions, R2's actual latency/throughput characteristics). Someone with real R2 credentials should run a manual backup/restore against a throwaway R2 bucket before depending on this in production. |
| SFTP backend | **Integration-proven** | `run-sftp.sh`: a throwaway `atmoz/sftp` server, key-based auth via a fresh itest-only Ed25519 keypair, `strict host-key checking disabled` (a real deployment should pin the real server's host key instead -- this is a throwaway itest server whose host key is regenerated every run). Found and fixed a real production gap: the shipped image had no `ssh` binary, and restic's sftp backend execs one to open the connection -- an `sftp:` destination was accepted as config but every operation against it would have failed with "executable file not found in $PATH". See "SFTP backend needs an ssh client, fixed" below. |
| `ballast.password-secret` override | **Integration-proven** | `run-password-secret.sh`: a real backup + restore round-trips through a service's named secret, and -- the part a mere round-trip can't prove, since it would also pass if the override silently did nothing -- direct `restic` invocations (same binary Ballast bundles) confirm the master-key-derived password (`ballast key`) does **not** open the repository while the named secret's own value does. |
| `ballast.repo.path` override | **Integration-proven** | `run-repo-path.sh`: a real backup lands at the overridden sub-path on the host-visible repos directory (a real restic `config` object found there) and explicitly does **not** exist at the default, un-overridden service-name path; restore round-trips through the same override. |
| Retention / forget, count-based (`keep-last`) | **Integration-proven, outcome asserted** | `run-retention.sh`: a service labeled `ballast.retention.last=3` is backed up five times (each real backup also runs `Forget` with that policy, exactly as production does — see `runBackupSteps`). The script records the snapshot ID each run produces and asserts the final surviving set is *exactly* the 3 newest (iterations 3, 4, 5), i.e. that `keep-last` forgets the right snapshots, not merely that `forget` exits 0. |
| Retention / forget, time-based, `keep-daily` specifically | **Integration-proven, at the engine level** | `run-retention-time.sh` runs `internal/engine/forget_time_itest_test.go`'s `TestForgetKeepDailyTimeBased` (build-tag `integration`) inside a throwaway container: `restic backup --time` seeds six snapshots across five distinct calendar days (two on one day, to prove the same-day-keeps-only-the-newest bucketing rule, not just a count), then the real `engine.Restic.Forget` — called through the `Engine` interface exactly as `internal/orchestrator/backup.go`'s `runBackupSteps` calls it — is applied with `RetentionPolicy{Daily: 3}`, and the exact surviving snapshot set is asserted by ID (the three most recent distinct days; the earlier of the two same-day snapshots is confirmed forgotten). This is at the engine level, not through Ballast's own CLI, because `BackupRequest` has no way to backdate a snapshot (nor should it) — see the file's own doc comment. |
| Retention / forget, time-based, `keep-hourly`/`keep-weekly`/`keep-monthly`/`keep-yearly`/`keep-within` | **Not asserted** | `keep-daily` is now proven at the engine level (row above); the other five time-based dimensions share the identical `internal/engine/restic.go` `Forget` code path (one `restic forget` invocation built from whichever `--keep-*`/`--keep-within` flags the policy sets) and the identical `TestForgetKeepDailyTimeBased` harness could extend to them, but nothing does yet. This is a smaller, more theoretical gap than before this pass (the code path itself is now exercised, just not every dimension of it), but still a real one. |
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

**Seventh bug, fixed this pass**: `config.Config.Splay` -- documented on
the struct itself as turning "the deterministic per-service splay of period
aliases ... on or off", parsed from `BALLAST_SPLAY` with its own env-overlay
code -- was never actually read by `internal/schedule` or `internal/daemon`:
the four period aliases (`@hourly`/`@daily`/`@weekly`/`@monthly`) were
always splayed regardless of the setting. This was found and deliberately
left as a documented gap by the prior pass (the analysis is still preserved
in git history), specifically because wiring a plain `bool` up naively would
have made the field's own zero value (`false`) silently *disable* the
anti-stampede splay by default, contradicting every doc comment describing
the feature -- a real design decision, not a mechanical fix, at the time.

Fixed this pass by changing `Splay` from `bool` to `*bool` in
`internal/config/config.go`: `nil` means "never set" and `applyDefaults`
resolves it to `true` (splay stays on by default, preserving today's
behavior exactly), while an explicit `splay: false` in `ballast.yml` or
`BALLAST_SPLAY=false` now actually reaches a concrete `false`.
`internal/schedule/scheduler.go`'s `Config` gained the identical `*bool`
field (same nil-means-true default), `Scheduler` resolves it once in `New`
into a concrete `splay bool`, and `schedule.Parse` gained a `splay bool`
parameter: when true, it behaves exactly as before (the four aliases land
on a job-name-derived slot inside the configured window); when false, it
falls through to `parseCron`, which -- for these particular strings --
means robfig/cron's own descriptor support, landing each alias on its
canonical, unsplayed boundary (top of the hour, midnight, Sunday midnight,
the 1st at midnight) instead. `internal/daemon/daemon.go`'s
`schedulerConfig` carries `cfg.Splay` straight through. See
`internal/config/config_test.go`, `internal/schedule/schedule_test.go`'s
`TestSplayFalse*`, and `internal/schedule/scheduler_test.go`'s
`TestNewDefaultsSplayOnWhenNil`/`TestNewHonorsExplicitSplayFalse` for the
tests proving both the default and the override actually take effect.

**Eighth bug, fixed this pass**: writing `run-sftp.sh` found that an
`sftp:user@host:path` destination -- accepted as config with no validation
at all, since `Destination.URL` is deliberately opaque, engine-native
syntax Ballast never parses -- could never actually work in the shipped
image. restic's sftp backend execs the `ssh` binary on `PATH` to open the
connection, and the production `Dockerfile`'s `alpine:3.20` runtime stage
installed nothing but the bundled `restic` binary itself, `ca-certificates`,
and `tzdata`. Every operation against a real `sftp:` destination would have
failed with "ssh: executable file not found in $PATH", silently (no
validation catches it; the failure only ever appears live, mid-backup).
Fixed by adding `openssh-client` to the `Dockerfile`'s `apk add` line, with
a comment explaining why it's there (Ballast itself never execs `ssh`;
restic does, underneath it). `run-sftp.sh` is what actually proves the fix:
a real backup and restore against a throwaway `atmoz/sftp` server, with
files confirmed to land on the server's own filesystem independent of
Ballast's view.

Writing `run-password-secret.sh`'s direct-`restic`-invocation proof (that
the master-key-derived password is correctly rejected, and the named
secret's own value is correctly accepted) found the exact same harness-only
bug `run-retention.sh` had already found and fixed once, in a different
script: a `:ro`-mounted repository directory makes any `restic` command
that takes a lock (which `restic snapshots` does by default, same as
`check`) hang forever retrying a lock-file write that can never succeed,
rather than surfacing a clean error. Not a bug in Ballast -- neither
`internal/engine/restic.go` nor any real Ballast operation ever needs to
run `restic` against a read-only repository mount -- but a real trap for
any one-off diagnostic `restic` invocation against a repo Ballast itself
manages. Fixed by adding `--no-lock` to `run-password-secret.sh`'s two
direct `restic snapshots` checks (the more correct fix here specifically,
since both are one-shot read-only diagnostics that don't need real lock
coordination at all, rather than dropping `:ro` the way `run-retention.sh`
did for its `check --read-data`, which does need to be able to write).

## What's still unproven, bluntly

- **Real R2.** MinIO proves the code path; it does not prove R2 itself. Run
  a manual backup/restore against a throwaway R2 bucket before trusting a
  production R2 destination.
- **Time-based retention beyond `keep-daily`** (`keep-hourly`/
  `keep-weekly`/`keep-monthly`/`keep-yearly`/`keep-within`). `keep-last` is
  proven with an exact surviving-set assertion (`run-retention.sh`), and
  `keep-daily` is now proven too, at the engine level, with synthetic
  timestamps (`run-retention-time.sh`, see the coverage matrix). The other
  five time-based dimensions share the identical `Forget` code path but
  have no test of their own extending the same technique to them.
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
  `tagwright.backup.*` alias. `retention.daily`'s *outcome* is now proven
  too, at the engine level (`run-retention-time.sh`), though not through a
  label -- it drives `engine.Restic.Forget` directly with a
  `RetentionPolicy{Daily: 3}` built by hand, not through
  `discovery.parseRetention` reading a `ballast.retention.daily` label off
  a container. `retention.hourly`/`.weekly`/`.monthly`/`.yearly`/`.within`/
  `.keep-tags` label parsing still has no test of its own, and none of the
  six dimensions is proven to parse correctly *from a label* the way
  `retention.last` is.
- **The indexed `ballast.exclude.<n>` escape hatch.** `run-volumes.sh`
  proves the CSV `ballast.exclude=<glob>` form end to end; the mutually
  exclusive indexed form (and the "setting both is a validation error" rule)
  has no itest of its own.
- **SFTP host-key pinning.** `run-sftp.sh` proves the SFTP backend and
  key-based auth work end to end, but disables strict host-key checking
  (`StrictHostKeyChecking no`, `UserKnownHostsFile /dev/null`) since the
  throwaway `atmoz/sftp` server's host key is regenerated every run. A real
  deployment should pin the real server's host key in its own `~/.ssh`
  config rather than copy the itest's disabled checking; nothing here
  proves Ballast (or restic) behaves correctly against a *mismatched* host
  key, only that a correctly-configured connection works.
