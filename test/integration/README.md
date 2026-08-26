# Ballast integration test harness

Real end-to-end tests of Ballast against a live Docker socket: build the
image, stand up throwaway labeled containers with known data, drive Ballast
through real backups, snapshot listings, and restores, diff the restored
data against the original, and tear everything down. Seventeen scripts,
each proving a different path:

- **`run.sh`** — filesystem backup/restore against a local repo: a single
  labeled container with known data in a named volume, backup, snapshot
  listing, restore, byte-diff, and (unless skipped) a daemon scheduler
  smoke test.
- **`run-stream.sh`** — the docker-exec-stdout-piped-into-`restic --stdin`
  path: a throwaway Postgres container with a known canary row, a
  `ballast.stream.db.*` labeled dump gated by `BALLAST_ENABLE_EXEC`,
  restore, byte-diff against the original dump, and a failure-path check
  (a bogus dump command must leave no snapshot behind).
- **`run-s3.sh`** — the restic S3 backend against a local MinIO standing in
  for Cloudflare R2 (an S3-compatible endpoint, same code path): a user
  network, a throwaway MinIO server and bucket, a labeled service backed
  up to it, confirming objects land in the bucket and the restore
  byte-matches.
- **`run-retention.sh`** — retention (`Forget`), prune, and check: a
  `ballast.retention.last=3` labeled service backed up five times, asserting
  the exact surviving snapshot set (not just a count); a real daemon with
  short prune/check schedules, confirmed clean from its own logs; and a
  direct `restic check --read-data` against the repository.
- **`run-hooks.sh`** — `ballast.exec.pre`/`ballast.exec.post`, gated by
  `BALLAST_ENABLE_EXEC`: both hooks write a distinct marker file into the
  volume being backed up; a `docker exec` confirms both actually ran, and
  restoring the snapshot proves the ordering directly (pre-marker present,
  post-marker absent). A second service with a failing `exec.pre` proves
  the run aborts with no snapshot while `exec.post` still runs.
- **`run-stop.sh`** — `ballast.stop=true`, gated by `BALLAST_ENABLE_STOP`:
  `docker events` watched across a real backup confirms the container was
  actually stopped and restarted, in that order, with a fresh
  `State.StartedAt`, and the snapshot taken while stopped restores
  byte-for-byte. Also confirms discovery rejects `stop=true` combined with
  a stream backup.
- **`run-notify.sh`** — live notification delivery through Ballast's own
  orchestrator: a real `binwiederhier/ntfy` server, a `notifications`
  channel pointed at it, and three real backups proving a message actually
  arrives (title, body, priority all checked via ntfy's JSON poll API),
  that `ballast.notify.suppress=true` produces none, and that
  `ballast.notify.on-success=true` escalates a successful backup's message
  to Warning-level.
- **`run-watch.sh`** — the daemon's live socket-event watch path
  (`internal/daemon/watch.go`), never exercised before this: a real
  `ballast daemon` started before a labeled container even exists, a real
  Docker "start" event discovering and backing it up, `docker rm -f`
  driving a real die+destroy event pair, "daemon: service unregistered" in
  the logs, and a full schedule interval afterward confirming no further
  backup fires.
- **`run-splay.sh`** — three services on the same schedule alias, proving
  `Concurrency=1` (the grammar's default) actually serializes their backups:
  each writes whole-second start/end markers around an artificial
  `exec.pre` delay into a shared host directory, and no two services'
  intervals overlap. Splay-slot distinctness for the same three service
  names is proven separately, at the unit level
  (`internal/schedule/schedule_test.go`), since a real `@daily` wait is
  impractical here.
- **`run-volumes.sh`** — multi-volume backup and narrowing: a service with
  two named volumes backs up both; `ballast.volumes=<name>` narrows to one;
  `ballast.volumes.exclude=<name>` excludes one; and a fourth service proves
  `ballast.exclude=<glob>` drops matching files and
  `ballast.exclude-caches=true` drops a real `CACHEDIR.TAG`-tagged
  directory's contents from the restored snapshot.
- **`run-dupe.sh`** — two containers resolving to the same service name via
  `ballast.name`: the daemon logs the rejection of the second, keeps
  running, and the latest snapshot after another schedule round still only
  ever contains the first container's data.
- **`run-alias.sh`** — a service labeled entirely under `tagwright.backup.*`
  (no `ballast.*` label at all): a real backup, snapshot tags, and restore,
  proving the org-namespaced alias works identically end to end.
- **`run-conflict.sh`** — a container labeling the same suffix differently
  under both prefixes (`ballast.repo=A` vs `tagwright.backup.repo=B`):
  rejected by the daemon's discovery pass (logged, never backed up) and by
  `ballast backup <service>`, which surfaces the real conflict instead of a
  misleading "not found".
- **`run-password-secret.sh`** — `ballast.password-secret=<name>`: a real
  backup + restore round-trip, plus the actual proof: direct `restic`
  invocations (the same binary Ballast bundles) confirm the master-key-
  derived password (`ballast key`) does **not** open the repository, while
  the named secret's own value does.
- **`run-repo-path.sh`** — `ballast.repo.path=<subpath>`: a real backup
  lands at the overridden sub-path on the host-visible repos directory, and
  explicitly does **not** exist at the default, un-overridden service-name
  path; restore round-trips through the same override.
- **`run-sftp.sh`** — the restic SFTP backend against a throwaway
  `atmoz/sftp` server: key-based auth via a fresh itest-only keypair, a
  real backup confirmed to have landed on the SFTP server's own filesystem,
  and a byte-matching restore. Found a real gap in the shipped image (no
  `ssh` binary) — see docs/TESTING.md's "Bugs found and fixed".
- **`run-retention-time.sh`** — not a container itest like the sixteen
  above: it runs `internal/engine`'s `TestForgetKeepDailyTimeBased`
  (build-tag `integration`) inside a throwaway `golang:1.25` + `restic`
  container, seeding a repository with snapshots at controlled synthetic
  times via `restic backup --time` and exercising the real
  `engine.Restic.Forget` with a `keep-daily` policy — the one thing no
  elapsed-time itest can practically prove.

See [docs/TESTING.md](../../docs/TESTING.md) for the full test methodology
and an honest coverage matrix across all of Ballast, not just this harness.

## What it touches

Every Docker object any of these scripts creates is named with the prefix
`ballast-itest-` (containers, volumes, networks) or tagged `ballast:itest`
(image). None of them ever starts, stops, execs into, or removes anything
else. Run them with `bash test/integration/<script>.sh`.

Each script cleans up everything it created on exit, including on failure
or interrupt (`--keep` skips this, for debugging). Each leaves the host as
it found it apart from its own generated files (repos, restores, secrets)
here, which are also removed on cleanup (except the generated
`repo-master-key`, reused across runs the same way `run.sh` reuses it) and
are gitignored so they never land in the repo regardless.

## Running them

```sh
bash test/integration/run.sh              # fs backup, snapshots, restore, daemon smoke test
bash test/integration/run.sh --skip-daemon # skip the daemon step (faster iteration)
bash test/integration/run-stream.sh        # stream (exec-to-stdin) backup/restore + failure path
bash test/integration/run-s3.sh            # S3 (MinIO) backup/restore
bash test/integration/run-retention.sh     # keep-last retention, prune, check
bash test/integration/run-hooks.sh         # exec.pre/exec.post hooks + failure path
bash test/integration/run-stop.sh          # ballast.stop stop-for-consistency + discovery rejection
bash test/integration/run-notify.sh        # live ntfy delivery + suppress/on-success controls
bash test/integration/run-watch.sh         # daemon socket-event watch: discover-and-add, remove-and-drop
bash test/integration/run-splay.sh         # multiple services, same schedule, Concurrency=1 serialization
bash test/integration/run-volumes.sh       # multi-volume backup, volumes narrowing, exclude/exclude-caches
bash test/integration/run-dupe.sh          # duplicate service name rejection
bash test/integration/run-alias.sh         # tagwright.backup.* alias end to end
bash test/integration/run-conflict.sh      # ballast.*/tagwright.backup.* prefix conflict rejection
bash test/integration/run-password-secret.sh # ballast.password-secret override, proven against the derived password
bash test/integration/run-repo-path.sh     # ballast.repo.path override lands at the overridden sub-path
bash test/integration/run-sftp.sh          # SFTP backend, real atmoz/sftp server
bash test/integration/run-retention-time.sh # keep-daily time-based retention, at the engine level
bash test/integration/<script>.sh --keep   # leave ballast-itest-* objects and generated files for inspection
```

All of them need a Docker socket at the default location and expect
`docker info --format '{{.DockerRootDir}}'` to report `/var/lib/docker`
(the default). If your host's Docker data root differs, add a `host_roots`
entry to the relevant `*.itest.yml` mapping the real volumes path to
itself before running (`run-stream.sh`, `run-notify.sh`, and `run-sftp.sh`
back up no filesystem paths at all, so this only matters for the rest).

`run-retention-time.sh` doesn't take `--keep`: it creates one throwaway
container (`ballast-itest-retention-time-runner`) that only ever holds a Go
test's own `t.TempDir()` state, already gone once the container exits, so
there is nothing left to inspect afterward beyond the test output it
already prints in full.

## Files

- `ballast.itest.yml`, `stream.itest.yml`, `s3.itest.yml`,
  `retention.itest.yml`, `hooks.itest.yml`, `stop.itest.yml`,
  `notify.itest.yml`, `watch.itest.yml`, `splay.itest.yml`,
  `volumes.itest.yml`, `dupe.itest.yml`, `alias.itest.yml`,
  `conflict.itest.yml`, `password-secret.itest.yml`, `repo-path.itest.yml`,
  `sftp.itest.yml` — committed. Minimal configs for each script: one
  destination each (`local` for all but `run-s3.sh`, which points
  `s3test` at the MinIO container, and `run-sftp.sh`, which points `sftp`
  at the atmoz/sftp container). None configure `notifications` except
  `notify.itest.yml`, whose whole point is exercising that config path
  against a real ntfy server; everywhere else the notifier falls back to
  beacon's built-in `log` channel and needs no secrets beyond the master
  key (and, for `run-s3.sh`, the MinIO credentials; `run-sftp.sh` needs no
  destination secrets at all, since its auth goes through a mounted SSH
  key, not a Ballast-resolved credential).
  `stream.itest.yml`/`hooks.itest.yml` are the ones that need
  `BALLAST_ENABLE_EXEC=true`, and `stop.itest.yml` needs
  `BALLAST_ENABLE_STOP=true`, each set by its script on the ballast run
  containers rather than in the config file, since those gates are meant
  to be scoped to exactly where they're used (`splay.itest.yml` follows the
  same pattern: its script sets `BALLAST_ENABLE_EXEC=true` on the daemon
  container only, not in the file). `run-retention-time.sh` has no
  `*.itest.yml` at all: it never runs the `ballast` binary, only a Go test
  against `engine.Restic` directly.
- `run.sh`, `run-stream.sh`, `run-s3.sh`, `run-retention.sh`,
  `run-hooks.sh`, `run-stop.sh`, `run-notify.sh`, `run-watch.sh`,
  `run-splay.sh`, `run-volumes.sh`, `run-dupe.sh`, `run-alias.sh`,
  `run-conflict.sh`, `run-password-secret.sh`, `run-repo-path.sh`,
  `run-sftp.sh`, `run-retention-time.sh` — committed. Each automates its
  own build, service/backend setup, backup, snapshots, restore or
  retention/prune/check, and cleanup.
- `secrets/repo-master-key` — generated, gitignored, shared by every
  script except `run-s3.sh` and `run-password-secret.sh` (each of which
  needs a secret alongside it beyond the master key, so each uses its own
  `secrets-*/` directory instead). A fresh `openssl rand -base64 32` master
  key, generated once and reused across runs.
- `secrets-s3/` — generated, gitignored. `run-s3.sh`'s own master key plus
  `r2-access-key-id`/`r2-secret-access-key` (the MinIO root credentials,
  named after the real R2 secret names so the destination config exercises
  the exact same secret-name wiring a real R2 destination uses).
- `secrets-password-secret/` — generated, gitignored. `run-password-secret.sh`'s
  own master key plus `svc-password`, the named secret
  `ballast.password-secret=svc-password` points at (deliberately not
  derivable from the master key, so the test's negative assertion — the
  derived password must NOT open the repository — actually means something).
- `ssh-sftp/` — generated, gitignored. `run-sftp.sh`'s fresh, itest-only
  Ed25519 keypair (regenerated every run) plus an `~/.ssh/config`
  disabling strict host-key checking against the throwaway server; mounted
  into every `ballast` invocation as `/root/.ssh`.
- `repos/`, `restore/`, `repos-stream/`, `restore-stream/`, `restore-s3/`,
  `repos-retention/`, `repos-hooks/`, `restore-hooks/`, `repos-stop/`,
  `restore-stop/`, `repos-notify/`, `repos-watch/`, `repos-splay/`,
  `markers-splay/`, `repos-volumes/`, `restore-volumes/`, `repos-dupe/`,
  `repos-alias/`, `restore-alias/`, `repos-conflict/`,
  `repos-password-secret/`, `restore-password-secret/`, `repos-repo-path/`,
  `restore-repo-path/`, `restore-sftp/` — generated, gitignored. The restic
  repositories and restore targets for each script's run (`run-s3.sh`
  writes to MinIO and `run-sftp.sh` writes to the SFTP server, neither a
  local repo path, so neither has a `repos-*/`; `run-retention.sh`,
  `run-notify.sh`, `run-watch.sh`, `run-dupe.sh`, and `run-conflict.sh`
  never restore into a local directory, so none of them has a
  `restore-*/` directory; `markers-splay/` is `run-splay.sh`'s shared
  bind-mounted host directory for its serialization-proof start/end marker
  files, not a restic repo at all; `run-retention-time.sh` has neither, its
  repository lives entirely inside its throwaway container's own
  `t.TempDir()`).
