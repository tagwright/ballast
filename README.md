# Ballast

Label-driven backups for Docker and Podman. Ballast watches the container
socket, reads `backup.*` labels off your running services, and drives restic to
back each one up. You describe what to back up in the compose file, next to the
service it belongs to, and Ballast handles the rest.

Ballast does not reimplement backups. restic does the encryption, the
deduplication, and the actual bytes on disk. Ballast owns discovery, scheduling,
and orchestration, so a bug in Ballast cannot corrupt a repository that restic
wrote. Each service gets its own repository, so a problem with one service stays
contained to it.

Status: early beta, and not everything below is wired up yet. restic is the only
engine so far, the Docker adapter comes before the Podman one, and the label
grammar is still being finalized. Pin a version if you build on it.

## The idea

Add a label to a service and it starts getting backed up:

```yaml
services:
  silverbullet:
    image: ghcr.io/silverbulletmd/silverbullet
    volumes:
      - sb-data:/space
    labels:
      backup.enable: "true"
```

That is the whole minimum. Ballast finds the `sb-data` volume, snapshots it on a
sensible daily schedule, and keeps a sane retention window. Everything past that
(where the repo lives, what to exclude, how to dump a database consistently, how
long to keep snapshots) is optional and set through more `backup.*` labels. Delete
the service and its schedule goes with it.

The full label reference is being designed and lands here once it settles.

## Secrets

Repository passwords and object-storage credentials never go in labels, where
`docker inspect` would print them in plaintext. Labels reference a secret by
name, and Ballast resolves the value at runtime from files that SOPS decrypts at
deploy time.

## License

GPL-3.0. You can run it, charge for it, and modify it. If you distribute a
modified version, it stays open under the same license. See [LICENSE](LICENSE).
