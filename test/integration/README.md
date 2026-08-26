# Ballast integration test harness

Real end-to-end tests of Ballast against a live Docker socket: build the
image, stand up throwaway labeled containers with known data, drive Ballast
through real backups, snapshot listings, and restores, diff the restored
data against the original, and tear everything down. Three scripts, each
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
bash test/integration/<script>.sh --keep   # leave ballast-itest-* objects and generated files for inspection
```

All three need a Docker socket at the default location and expect
`docker info --format '{{.DockerRootDir}}'` to report `/var/lib/docker`
(the default). If your host's Docker data root differs, add a `host_roots`
entry to the relevant `*.itest.yml` mapping the real volumes path to
itself before running (`run-stream.sh` backs up no filesystem paths at
all, so this only matters for `run.sh` and `run-s3.sh`).

## Files

- `ballast.itest.yml`, `stream.itest.yml`, `s3.itest.yml` — committed.
  Minimal configs for each script: one destination each (`local` for the
  first two, `s3test` pointing at the MinIO container for the third), no
  notifications or telemetry (so the notifier falls back to beacon's
  built-in `log` channel and needs no secrets beyond the master key and,
  for `run-s3.sh`, the MinIO credentials). `stream.itest.yml` is the only
  one that needs `BALLAST_ENABLE_EXEC=true`, set by `run-stream.sh` on the
  ballast run containers rather than in the config file, since that gate
  is meant to be scoped to exactly where streams/hooks are used.
- `run.sh`, `run-stream.sh`, `run-s3.sh` — committed. Each automates its
  own build, service/backend setup, backup, snapshots, restore, diff, and
  cleanup.
- `secrets/repo-master-key` — generated, gitignored, shared by `run.sh`
  and `run-stream.sh`. A fresh `openssl rand -base64 32` master key,
  generated once and reused across runs.
- `secrets-s3/` — generated, gitignored. `run-s3.sh`'s own master key plus
  `r2-access-key-id`/`r2-secret-access-key` (the MinIO root credentials,
  named after the real R2 secret names so the destination config exercises
  the exact same secret-name wiring a real R2 destination uses).
- `repos/`, `restore/`, `repos-stream/`, `restore-stream/`, `restore-s3/`
  — generated, gitignored. The restic repositories and restore targets for
  each script's run (`run-s3.sh` writes to MinIO, not a local repo path,
  so it has no `repos-s3/`).
