# Machine-readable records

Ballast writes one JSON document per backup run (a `ballast.run.v1` record) and
one per verify (a `ballast.verify.v1` record). They are the source of truth
about what a run or a verify did, meant to be read by machines, not just tailed
in a log. The compliance tooling in the wider tagwright suite consumes these
records, but the format stands on its own and needs nothing external to read.

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

The `ballast.run.v1` and `ballast.verify.v1` schemas are frozen and live in the
billet-evidence repository, not in this one, alongside `common.v1.json` and
worked golden examples:

```
billet-evidence/schema/ballast/run.v1.json
billet-evidence/schema/ballast/verify.v1.json
billet-evidence/schema/ballast/common.v1.json
billet-evidence/golden/ballast/run-pass.json
billet-evidence/golden/ballast/run-fail.json
billet-evidence/golden/ballast/verify-pass.json
billet-evidence/golden/ballast/verify-fail.json
billet-evidence/golden/ballast/verify-inconclusive.json
```

`internal/record` in this repository is the Go mirror of those schemas, and its
output validates against them. The schema JSON is intentionally not duplicated
here.

## The verify record: ballast.verify.v1

`ballast verify <service>` writes one `ballast.verify.v1` document per
invocation, the machine-readable proof that a snapshot restores. It goes to:

```
<state_dir>/verifies/<service>/<verify_id>.json
```

and, with `ballast verify <service> --json`, to stdout as well (stdout carries
only the record in that mode). `verify_id` is a ULID, like `run_id`. The record
keys its evidence on the same stable `host_id`, so a verify is refused if that
identity cannot be resolved.

Field by field (see the schema for exact types and the null rules):

- `record` is the constant `ballast.verify.v1`.
- `verify_id`, `host_id`, `service`, `runtime`, `runtime_ref`, `repo_id`,
  `trigger`, `requested_by`, `engine`, `ballast_version` mean exactly what they
  do on a run record. `ballast verify --requested-by <who>` sets `trigger` to
  `remote` and `requested_by` to `<who>`; without it the local CLI path records
  `manual` and a null `requested_by`.
- `snapshot_requested` is the `--snapshot` argument as given (`latest` or an
  id); `snapshot_id` and `snapshot_time` are the snapshot actually resolved and
  restored, `null` only when none could be resolved (`snapshot_missing`).
- `mode` is the one closed enum on this seam: `files`, `container`, or
  `stream-restore`.
- `dataset` is human text describing what was restored; `restored` is its
  machine form: `{kind: stream|volumes|paths, items}`.
- `data_engine`, `image`, `probe`, `expect` are the operator's free-string
  configuration echoed back. `data_engine` is informational and never
  load-bearing; `image` is `null` in files mode.
- `assertion` names what decided the outcome: `manifest`, `probe`, or
  `probe_expect` (forced to `manifest` when there is no probe).
- `timeout_ms` is the effective wall clock: the `verify.timeout` label (or its
  10m default), or the `ballast verify --timeout` override when one was given.
- `environment` records where the restore ran: `{kind: scratch-dir |
  throwaway-container, location, image, network, network_isolated}`.
  `network_isolated` is the segregation fact, recorded `true` because a
  throwaway container is always placed on an internal network (confirmed via
  the runtime's network inspector).
- `started_at`, `finished_at`, and the three `*_duration_ms` fields time the
  verify; the total is judged for RTO and the parts (restore, probe) for
  tuning.
- `result` is `pass`, `fail`, or `inconclusive`. `reason_code` is a closed
  classification and `reason` its human text: a pass carries neither, a
  non-pass carries both. `inconclusive` (its codes: `probe_timeout`,
  `restore_failed`, `restore_timeout`, `snapshot_missing`, `image_unavailable`,
  `scratch_unavailable`, `runtime_unavailable`, `cancelled`, `other`) is never
  a pass anywhere downstream.
- `checked` is an open integer map; `files`, `bytes`, `rows` are documented
  keys, and any other (`documents`, `objects`, ...) is permitted.
- `manifest_compare` is the files-mode diff against the backup-time manifest
  (`entries_expected`, `entries_matched`, `mismatched`, `missing`, `extra`),
  `null` in the other modes.
- `probe_output` is a bounded excerpt of the probe's stdout (4096 bytes) plus a
  SHA-256 over the full stdout, `null` when no probe ran.
- `scratch_destroyed` and `scratch_destroy_error` report the teardown: a leaked
  scratch copy of production data is a finding in its own right, so cleanup is
  part of the evidence.

Only the probe (or, in probe-less files mode, the manifest diff) turns a verify
into `pass` or `fail`. Everything that stops the assertion from running at all,
a missing snapshot, an unpullable image, a failed or timed-out restore, is
`inconclusive`, never a silent pass.
