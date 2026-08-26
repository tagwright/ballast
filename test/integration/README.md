# Ballast integration test harness

Real end-to-end tests of Ballast against a live Docker socket: build the
image, stand up throwaway labeled containers with known data, drive Ballast
through real backups, snapshot listings, and restores, diff the restored
data against the original, and tear everything down. Seven scripts, each
proving a different path:

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
bash test/integration/<script>.sh --keep   # leave ballast-itest-* objects and generated files for inspection
```

All of them need a Docker socket at the default location and expect
`docker info --format '{{.DockerRootDir}}'` to report `/var/lib/docker`
(the default). If your host's Docker data root differs, add a `host_roots`
entry to the relevant `*.itest.yml` mapping the real volumes path to
itself before running (`run-stream.sh` and `run-notify.sh` back up no
filesystem paths at all, so this only matters for the rest).

## Files

- `ballast.itest.yml`, `stream.itest.yml`, `s3.itest.yml`,
  `retention.itest.yml`, `hooks.itest.yml`, `stop.itest.yml`,
  `notify.itest.yml` — committed. Minimal configs for each script: one
  destination each (`local` for all but `run-s3.sh`, which points
  `s3test` at the MinIO container). None configure `notifications` except
  `notify.itest.yml`, whose whole point is exercising that config path
  against a real ntfy server; everywhere else the notifier falls back to
  beacon's built-in `log` channel and needs no secrets beyond the master
  key (and, for `run-s3.sh`, the MinIO credentials).
  `stream.itest.yml`/`hooks.itest.yml` are the ones that need
  `BALLAST_ENABLE_EXEC=true`, and `stop.itest.yml` needs
  `BALLAST_ENABLE_STOP=true`, each set by its script on the ballast run
  containers rather than in the config file, since those gates are meant
  to be scoped to exactly where they're used.
- `run.sh`, `run-stream.sh`, `run-s3.sh`, `run-retention.sh`,
  `run-hooks.sh`, `run-stop.sh`, `run-notify.sh` — committed. Each
  automates its own build, service/backend setup, backup, snapshots,
  restore or retention/prune/check, and cleanup.
- `secrets/repo-master-key` — generated, gitignored, shared by every
  script except `run-s3.sh` (which needs MinIO credentials alongside it,
  so it uses its own `secrets-s3/`). A fresh `openssl rand -base64 32`
  master key, generated once and reused across runs.
- `secrets-s3/` — generated, gitignored. `run-s3.sh`'s own master key plus
  `r2-access-key-id`/`r2-secret-access-key` (the MinIO root credentials,
  named after the real R2 secret names so the destination config exercises
  the exact same secret-name wiring a real R2 destination uses).
- `repos/`, `restore/`, `repos-stream/`, `restore-stream/`, `restore-s3/`,
  `repos-retention/`, `repos-hooks/`, `restore-hooks/`, `repos-stop/`,
  `restore-stop/`, `repos-notify/` — generated, gitignored. The restic
  repositories and restore targets for each script's run (`run-s3.sh`
  writes to MinIO, not a local repo path, so it has no `repos-s3/`;
  `run-retention.sh` and `run-notify.sh` never restore, so neither has a
  `restore-*/` directory).
