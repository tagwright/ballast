# Ballast

Label-driven backups for Docker Compose services. Ballast watches the
container socket, reads `ballast.*` labels off your running services, and
drives restic to back each one up. You describe what to back up in the
compose file, next to the service it belongs to, and Ballast handles the
rest.

Ballast does not reimplement backups. restic does the encryption, the
deduplication, and the actual bytes on disk. Ballast owns discovery, scheduling,
and orchestration, so a bug in Ballast cannot corrupt a repository that restic
wrote. Each service gets its own repository, so a problem with one service stays
contained to it.

Status: beta. See [Status](#status) below for what's built and what's not yet.

## The idea

Add a label to a service and it starts getting backed up:

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    volumes:
      - sb-data:/space
    labels:
      ballast.enable: "true"
```

That is the whole minimum. Ballast finds the `sb-data` volume, snapshots it on a
sensible daily schedule, and keeps a sane retention window. Everything past that
(where the repo lives, what to exclude, how to dump a database consistently, how
long to keep snapshots) is optional and set through more `ballast.*` labels. Delete
the service and its schedule goes with it.

The full label reference is in [docs/LABELS.md](docs/LABELS.md).

## Secrets

Repository passwords and object-storage credentials never go in labels, where
`docker inspect` would print them in plaintext. Labels reference a secret by
name, and Ballast resolves the value at runtime from files that SOPS decrypts at
deploy time.

## Deploy

Ballast runs as one container, alongside the services it backs up in the same
compose file (or stack). It needs the container socket to discover and inspect
services, the Docker volumes root to read what it backs up, and a secrets
directory that SOPS fills in at deploy time. All three mounts are read-only:

```yaml
services:
  ballast:
    image: ghcr.io/tagwright/ballast:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/lib/docker/volumes:/var/lib/docker/volumes:ro
      - ./ballast.yml:/etc/ballast/ballast.yml:ro
      - /run/ballast/secrets:/run/ballast/secrets:ro
    command: ["daemon", "--config", "/etc/ballast/ballast.yml"]
```

The `/var/lib/docker/volumes` mount matters: Ballast maps a named volume's
host-side path back to itself by default, so mounting it at the same path
inside the Ballast container is what makes named-volume discovery work with
zero configuration. See [ballast.example.yml](ballast.example.yml) if you
also bind-mount host directories from outside the Docker volumes root; those
need an explicit `host_roots` entry.

A service opts in the same way regardless of where Ballast itself runs, with
labels next to the service they describe:

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    volumes:
      - sb-data:/space
    labels:
      ballast.enable: "true"
      ballast.repo: r2
      ballast.retention.daily: "7"
      ballast.retention.weekly: "4"
      ballast.retention.monthly: "6"
```

Per-service retention is a set of flat `ballast.retention.<dimension>`
labels (`last`, `hourly`, `daily`, `weekly`, `monthly`, `yearly`, each a
plain integer), plus `ballast.retention.within` (a restic duration, e.g.
`30d`) and `ballast.retention.keep-tags`. A service that sets none of these
falls back to the global `retention:` default in `ballast.yml` (or
`daily=7,weekly=4,monthly=6` if that is unset too). See
[docs/LABELS.md](docs/LABELS.md) for the complete list.

Secrets never live in the compose file or the labels. SOPS decrypts them at
deploy time into the directory mounted at `/run/ballast/secrets`, one file per
secret name, and `ballast.yml` and any `ballast.*` label reference a secret by
that name. The repository master key, which every service's per-repo password
derives from, is the secret named `repo-master-key`.

See [ballast.example.yml](ballast.example.yml) for a full annotated config.

## Verify it worked

Two commands confirm a service is set up correctly without waiting for its
scheduled run:

Force a backup now, exactly as the scheduler would run it (pre-hook, optional
stop, filesystem and stream backups, restart, retention, post-hook, and a
notification):

```
ballast backup silverbullet
```

If the service isn't found, the error tells you why: either no running
container currently has `ballast.enable=true` under that name, or discovery
found it but rejected it (a bad label value, a prefix conflict between
`ballast.*` and `tagwright.backup.*`, or a primitive like `stop` or a stream
used without its global gate). Fix what it names and try again.

Then confirm the snapshot landed:

```
ballast snapshots silverbullet
```

This lists every snapshot in the service's repository: ID, time, host, tags,
and paths. A fresh row with the current timestamp means discovery, the
repository, and the retention policy are all wired up correctly. Run
`ballast snapshots` with no argument to list every currently discoverable
enabled service at once.

## Secrets with SOPS

This is a minimal, real recipe: generate an age key, encrypt a small secrets
file with it, and decrypt it into `/run/ballast/secrets` at deploy time so
Ballast can resolve `ballast.password-secret`, destination credentials, and
notification tokens by name.

1. Generate an age key pair. Keep the private key off the host it protects,
   somewhere durable:

   ```
   age-keygen -o ballast-age-key.txt
   ```

   This prints the matching public key (`age1...`) to stderr; keep both the
   private key file and that public value.

2. Point SOPS at that public key with a `.sops.yaml` in the repo (or
   directory) holding your secrets file, so `sops` knows who can decrypt it:

   ```yaml
   creation_rules:
     - path_regex: \.sops\.env$
       age: age1exampleexampleexampleexampleexampleexampleexampleexamplex
   ```

3. Write the secret values as `NAME=value` pairs in a plain file, then
   encrypt it in place:

   ```
   cat > secrets.sops.env <<'EOF'
   repo-master-key=<output of: openssl rand -base64 32>
   r2-access-key-id=<your R2 access key>
   r2-secret-access-key=<your R2 secret key>
   ntfy-ballast-token=<your ntfy access token>
   EOF

   sops -e -i secrets.sops.env
   ```

   `secrets.sops.env` now holds ciphertext and is safe to commit. Each key on
   the left is a secret *name*, the same name a destination's `env` map or a
   `password-secret` label references, never the value itself.

4. At deploy time, decrypt it into one file per secret name under
   `/run/ballast/secrets`, the directory Ballast's `secrets_dir` (default
   `/run/ballast/secrets`) reads from. A small init step run before `ballast
   daemon` starts does this:

   ```sh
   #!/bin/sh
   set -eu
   mkdir -p /run/ballast/secrets
   sops -d secrets.sops.env | while IFS='=' read -r name value; do
     [ -n "$name" ] || continue
     printf '%s' "$value" > "/run/ballast/secrets/$name"
     chmod 600 "/run/ballast/secrets/$name"
   done
   ```

   Wire this as the compose service's entrypoint (running it before `exec
   ballast daemon ...`), or as a one-shot init container that writes into the
   same volume the `ballast` service mounts read-only at
   `/run/ballast/secrets`. Either way, the age private key from step 1 needs
   to be available wherever this decrypt step runs (as `SOPS_AGE_KEY_FILE`,
   typically), and nowhere else.

Ballast never reads `.sops.env` files directly and never runs `sops` itself:
by the time it starts, the secrets directory already holds plaintext files
named after each secret, and that resolution (file first, then a
`BALLAST_SECRET_<NAME>` environment variable) is all it does.

## Status

Ballast is built and running: discovery, scheduling, restic-backed backup and
restore, retention, and notifications all work end to end. What's not there
yet:

- The Docker adapter is the only container runtime supported. A Podman
  adapter is planned but not built.
- restic is the only backup engine. The engine interface is designed to hold
  a second one, but nothing else implements it yet.

Pin a version if you build on it; the label grammar can still change before
a 1.0.

## License

GPL-3.0-or-later. You can run it, charge for it, and modify it. If you distribute a
modified version, it stays open under the same license. Each source file carries an
`SPDX-License-Identifier: GPL-3.0-or-later` header. See [LICENSE](LICENSE).
