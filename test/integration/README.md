# Ballast integration test harness

This is the first real end-to-end test of Ballast against a live Docker
socket: it builds the image, stands up a single throwaway labeled
container with known data in a named volume, drives Ballast through a
real backup, a snapshot listing, and a restore, diffs the restored data
against the original, and (unless skipped) smoke-tests the daemon's
scheduler by letting it fire one backup on its own.

## What it touches

Every Docker object this harness creates is named with the prefix
`ballast-itest-` (container, volume) or tagged `ballast:itest` (image).
It never starts, stops, execs into, or removes anything else. Run it with
`bash test/integration/run.sh`.

`run.sh` cleans up everything it created on exit, including on failure or
interrupt (`--keep` skips this, for debugging). It leaves the host as it
found it apart from the generated files under `repos/`, `restore/`, and
`secrets/` here, which are also removed on cleanup and are gitignored so
they never land in the repo regardless.

## Running it

```sh
bash test/integration/run.sh              # full run: backup, snapshots, restore, daemon smoke test
bash test/integration/run.sh --skip-daemon # skip the daemon step (faster iteration)
bash test/integration/run.sh --keep        # leave ballast-itest-* objects and generated files in place for inspection
```

It needs a Docker socket at the default location and expects
`docker info --format '{{.DockerRootDir}}'` to report `/var/lib/docker`
(the default). If your host's Docker data root differs, add a
`host_roots` entry to `ballast.itest.yml` mapping the real volumes path
to itself before running.

## Files

- `ballast.itest.yml` — committed. One destination (`local`, pointing at
  `/repos` inside the Ballast container), no notifications or telemetry
  (so the notifier falls back to beacon's built-in `log` channel and
  needs no secrets), exec and stop left at their default-disabled
  settings.
- `run.sh` — committed. Automates build, service setup, backup,
  snapshots, restore, diff, and the daemon smoke test.
- `secrets/repo-master-key` — generated, gitignored. A fresh
  `openssl rand -base64 32` master key each run (run.sh only generates
  one if it isn't already there).
- `repos/`, `restore/` — generated, gitignored. The restic repository and
  restore target for the test run.
