# Machine-readable run records

Ballast writes one JSON document per backup run: a `ballast.run.v1` record.
It is the source of truth about what a run did, meant to be read by machines,
not just tailed in a log. The compliance tooling in the wider tagwright suite
consumes these records, but the format stands on its own and needs nothing
external to read.

## Where records go

Every run writes its record to:

```
<state_dir>/runs/<service>/<run_id>.json
```

`state_dir` defaults to `/var/lib/ballast` and is set by `state_dir` in
`ballast.yml` or `BALLAST_STATE_DIR`. `run_id` is a ULID (26 Crockford base32
characters), sortable by time, so a service's records list in run order.

`ballast backup <service> --json` additionally prints the same record to
stdout. In that mode stdout carries only the record: the human "backup
complete" line is suppressed, and Ballast's own logs stay on stderr (switch
them to structured output with `log.format: json`, see `ballast.example.yml`).

Records are only written once a run can be attributed to a host: they carry
the stable `host_id` (see `ballast identity`), which is generated once and
persisted in the state directory. If that identity cannot be resolved,
recording is skipped rather than writing a record with no valid host.

## The host identity

`host_id` is the string `h_` followed by 4 to 64 lowercase hex characters,
drawn once from a CSPRNG and persisted at `<state_dir>/host_id`. It is stable
across container recreation, which is what lets a fleet view join a host's
records over time. `ballast identity` prints it (generating it on first run).
A corrupt or unreadable `host_id` file is an error, never silently replaced,
so a host's identity cannot drift out from under the records already keyed on
it.

## What a record contains

Field by field (see the schema for exact types and the null rules):

- `record` is the constant `ballast.run.v1`, the discriminator a reader
  dispatches on.
- `run_id`, `host_id`, `service` identify the run, the host, and the service.
- `runtime` and `runtime_ref` are the runtime seam: `runtime` is the engine
  (`docker` or `podman`), and `runtime_ref` is an open map carrying
  `container_name`, `container_id`, and `compose_project` when the service
  belongs to a compose project.
- `repo_id` is the destination name and the repository path within it, no
  credentials. `repo_properties` reports what Ballast can actually assess
  about the repository: `backend` (read from the destination URL),
  `encrypted` (always true, a fact of the restic engine), and `offsite` and
  `immutable` left `null` because Ballast will not infer them from a URL. A
  consumer treats `null` as not assessed, never as false.
- `trigger` is what started the run (`schedule`, `manual`, `event`,
  `remote`), and `requested_by` names a remote requester when there is one.
- `snapshot_id` and `snapshot_time` describe the snapshot the run produced,
  and are both `null` when it produced none. `snapshot_time` is the RPO
  reference point.
- `started_at`, `finished_at`, `duration_ms` time the run. Timestamps are
  RFC 3339 UTC with a literal `Z`; durations are integer milliseconds.
- `paths` are the host-visible filesystem paths backed up (empty for a
  stream-only service). `bytes_added` and `files_new` are the engine's own
  accounting; `bytes_processed` and `files_processed` are the totals scanned,
  `null` when no snapshot was produced.
- `streams[]` is one entry per `stream.<id>` label, each with its own
  `bytes`, `exit`, `duration_ms`, and `error`, because a stream can fail
  while the filesystem pass succeeds.
- `hooks.pre` and `hooks.post` are the exec-hook outcomes, `null` when the
  hook was not declared.
- `stopped_for_backup` says whether `ballast.stop` stopped the workload for
  the filesystem pass.
- `retention` is the forget pass outcome, `null` when it did not run.
- `manifest` is the backup-time manifest handle (see below), `null` when none
  was recorded.
- `exit` and `error` are the run's outcome. `exit` is `0` only when the run
  produced a snapshot with no error; a run that fails, or that produces no
  snapshot at all, is a non-zero `exit` with an `error`.
- `engine` names the backup engine and its version (`restic` today), and
  `ballast_version` is the Ballast build.

## The backup-time manifest

When a service has any `verify.*` label configured, and it has a filesystem
tree to back up, Ballast records a manifest of that tree at backup time: one
entry per file with its path, size, and SHA-256, hashed while the workload is
quiesced so it reflects exactly what the snapshot captured. The record's
`manifest` field is the handle to it: `entries`, `bytes`, a `digest` over the
manifest file, and its `location` under the state directory. A later
files-mode verify diffs a restored snapshot against this manifest.

The manifest costs a full hash pass over the volume, so it is deliberately
opt-in by the presence of verify configuration. A service with no verify
config records `manifest: null` and runs no hash pass.

## The canonical schema

The `ballast.run.v1` schema is frozen and lives in the billet-evidence
repository, not in this one, alongside `common.v1.json` and worked golden
examples:

```
billet-evidence/schema/ballast/run.v1.json
billet-evidence/schema/ballast/common.v1.json
billet-evidence/golden/ballast/run-pass.json
billet-evidence/golden/ballast/run-fail.json
```

`internal/record` in this repository is the Go mirror of that schema, and its
output validates against it. The schema JSON is intentionally not duplicated
here.

## Coming next: ballast.verify.v1

A companion `ballast.verify.v1` record, one per `ballast verify` invocation,
is defined in the same frozen schema set and will be written by the verify
command when it lands. The manifest recorded here is the foundation a
files-mode verify diffs against. This document will grow to cover it.
