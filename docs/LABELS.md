# Label reference

This is the complete set of labels Ballast reads off a container, generated
from the label grammar the code actually implements
(`internal/discovery/*.go`).

## Prefixes

Every label below is written with the `ballast.` prefix. `tagwright.backup.`
is accepted as an identical alias for every one of them: `ballast.repo` and
`tagwright.backup.repo` mean exactly the same thing. `backup.*` (no
namespace) is **not** recognized and is silently ignored, same as any other
unrecognized label.

You can mix the two prefixes on the same service, but not on the same key:
setting both `ballast.repo` and `tagwright.backup.repo` to the *same* value
is fine, and setting them to *different* values is a validation error (the
service is skipped and the conflict is reported), since there is no silent
precedence between the two.

## Table

| Label | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Opts the service in. Every other label is ignored unless this is `true`. |
| `name` | string | compose service name, else container name | Pins the identity used for service naming, repository path, and the master-key-derived repo password. Set this before renaming a service if you want to keep its backup history; see [docs/RECOVERY.md](RECOVERY.md). |
| `repo` | string | `default_destination` from `ballast.yml` | Selects a destination by name from the `destinations` map in `ballast.yml`. |
| `repo.path` | string | resolved service name | Sub-path within the destination's repository namespace. |
| `password-secret` | string (secret name) | unset | Names a secret holding this service's repository password directly, bypassing master-key derivation. |
| `volumes` | CSV of tokens, or `none` | all eligible mounts | Restricts filesystem discovery to the listed mounts. A bare token matches a named volume's name; a token starting with `/` matches a mount's container-side destination. `none` disables filesystem backup for this service entirely (streams and hooks still run). |
| `volumes.exclude` | CSV of tokens | none excluded | Same token grammar as `volumes`, but drops matches instead of selecting them. Applied after `volumes` narrows the set. |
| `exclude` | CSV of restic glob patterns | none | Paths to exclude from the backup. Mutually exclusive with `exclude.<n>`; setting both is a validation error. |
| `exclude.<n>` | string, indexed (`exclude.0`, `exclude.1`, ...) | none | Escape hatch for a single exclude pattern that itself contains a comma. Ascending index order; mutually exclusive with `exclude`. |
| `exclude-caches` | bool | `true` | Honors `CACHEDIR.TAG` to skip cache directories. |
| `retention.last` | int | `0` (unset) | Keep the last N snapshots regardless of time bucket. |
| `retention.hourly` | int | `0` (unset) | Keep N hourly snapshots. |
| `retention.daily` | int | `0` (unset) | Keep N daily snapshots. |
| `retention.weekly` | int | `0` (unset) | Keep N weekly snapshots. |
| `retention.monthly` | int | `0` (unset) | Keep N monthly snapshots. |
| `retention.yearly` | int | `0` (unset) | Keep N yearly snapshots. |
| `retention.within` | string (restic duration, e.g. `30d`) | unset | Keep every snapshot within this duration of now, in addition to the bucketed rules above. |
| `retention.keep-tags` | CSV | none | Snapshots carrying any of these tags are never forgotten. |
| `stream.<id>.command` | string, indexed by `<id>` | required if any `stream.<id>.*` label is set | Shell command run inside the service's container; its stdout is piped straight into the engine as one stdin snapshot entry. Requires `enable_exec: true` (or `BALLAST_ENABLE_EXEC=true`). |
| `stream.<id>.filename` | string | `<id>` | Name the stream's snapshot entry is stored under. |
| `stream.<id>.user` | string | container's default user | User the stream command runs as. |
| `stream.<id>.timeout` | Go duration (e.g. `5m`) | `15m` | How long the stream command may run before it's killed. |
| `exec.pre` | string | unset (hook not run) | Command run inside the service's container before the backup starts. Requires `enable_exec: true`. |
| `exec.pre.timeout` | Go duration | `5m` | Timeout for `exec.pre`. |
| `exec.pre.user` | string | container's default user | User `exec.pre` runs as. |
| `exec.post` | string | unset (hook not run) | Command run inside the service's container after the backup finishes. Requires `enable_exec: true`. |
| `exec.post.timeout` | Go duration | `5m` | Timeout for `exec.post`. |
| `exec.post.user` | string | container's default user | User `exec.post` runs as. |
| `stop` | bool | `false` | Stops the container for the duration of the filesystem backup, restarting it afterward. Requires `enable_stop: true` (or `BALLAST_ENABLE_STOP=true`). Incompatible with any `stream.<id>.*` label on the same service (a stopped container can't run a stream command). |
| `schedule` | cron expression or alias (e.g. `@daily`) | global `schedule` from `ballast.yml` (itself defaulting to `@daily`) | This service's own backup schedule. |
| `tags` | CSV | none | Extra tags appended to Ballast's own automatic tags on every snapshot for this service. |
| `notify.suppress` | bool | `false` | Mutes this service's alert-channel notifications (both success and failure). The Gatus-style telemetry/health push still runs, since it isn't an alert. |
| `notify.on-success` | bool | `false` | Notifies this service's successful backups at Warning level instead of Info, so successes surface on channels configured to only forward warnings and errors. Failures already notify at Error and are unaffected. |

## Notes

- **Booleans** are parsed with `strconv.ParseBool`: `"true"`/`"false"`,
  `"1"`/`"0"`, `"t"`/`"f"` all work. An absent or empty label falls back to
  the column above, not to `false`.
- **Retention is a replace, not a merge.** If a service sets *any*
  `retention.*` label, that service's policy is exactly what its labels say;
  the dimensions it doesn't set stay unset (not filled in from the global
  default). The global `retention:` policy from `ballast.yml` only applies
  when a service sets *no* retention label at all. See the global-config
  format in [ballast.example.yml](../ballast.example.yml).
- **Streams and hooks are gated globally.** `enable_exec: true` (or
  `BALLAST_ENABLE_EXEC=true`) must be set in `ballast.yml`/env before any
  `stream.*` or `exec.*` label takes effect; without it, a service that sets
  one is rejected with a validation error rather than silently ignored.
  Same for `stop` and `enable_stop`.
- Everything else Ballast needs (which destination a repo lives at, alert
  channels, `host_roots`, global excludes) is set once in `ballast.yml`, not
  per label. See [ballast.example.yml](../ballast.example.yml) for that side
  of the configuration.
- **The compose service/project fallback in `name` works the same under
  Podman.** podman-compose (and `podman compose`) label containers with the
  same `com.docker.compose.project`/`com.docker.compose.service` pair Docker
  uses, so `ballast.name`'s fallback to the compose service name resolves
  identically regardless of which runtime is selected. See the Podman
  section of [README.md](../README.md) for selecting the runtime itself,
  which is a `ballast.yml`/env setting, not a label.
- **Per-service channel routing is a possible future addition**, not
  implemented today: `notify.suppress` and `notify.on-success` only control
  whether and at what level a service notifies, not which configured beacon
  channel(s) receive it. Every channel in `ballast.yml` still sees every
  non-suppressed notification.
