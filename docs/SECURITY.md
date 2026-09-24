<!-- SPDX-License-Identifier: GPL-3.0-or-later -->
# Security

Ballast drives backups, so the trust argument is about one key and one boundary:
the master key every repository password derives from, and the fact that the
encryption itself is restic's rather than Ballast's. This document states what
Ballast stores, what it never stores, and where recovery depends on you. Every
claim here is checkable against the code, the recovery walkthrough in
[RECOVERY.md](RECOVERY.md), and the coverage matrix in [TESTING.md](TESTING.md).

## Ballast does not do the crypto

restic does the encryption, the deduplication, and the bytes on disk. Ballast
owns discovery, scheduling, and orchestration. It calls restic to write and read
repositories, and it never reimplements any of what restic does. A bug in Ballast
cannot corrupt a repository that restic wrote, because Ballast is not the thing
writing the repository format. Each service also gets its own repository, so a
problem confined to one service's repository stays confined to it.

What this means for the threat model: the confidentiality of your backups is
restic's encryption, at restic's strength, and Ballast's job is to hand restic
the right password and never to leak it.

## The master key, and the one secret Ballast stores

Ballast stores exactly one secret: the master key, named `repo-master-key`. It
does not store a password per repository. Every service's restic repository
password is derived from the master with HKDF-SHA256, keyed on the service name.
The derivation is deterministic, so the same master and the same service name
always produce the same password, on any machine, with no per-service state
written anywhere.

That design has a security consequence stated plainly: the master key plus a
service name is enough to recover that repository's password. There is no
database, no per-service credential file, nothing else Ballast wrote that a
recovery depends on. It also means the master is the single most sensitive thing
Ballast touches, more sensitive than any one repository, because every repository
ultimately hangs off it.

The master itself never lives in a label or in `ballast.yml`, where `docker
inspect` would print it. Labels and config reference secrets by name. The value
resolves at runtime, file-first from the secrets directory (default
`/run/ballast/secrets`) and then from a `BALLAST_SECRET_<NAME>` variable, from
files that SOPS decrypts at deploy time. Ballast never runs `sops` itself and
never reads a `.sops.env` file. By the time it starts, the secrets directory
already holds plaintext files, and resolving a name to a value is all Ballast
does. The age key that decrypts the SOPS source lives wherever that decrypt step
runs, and nowhere else. No secret value is ever logged. A too-short or missing
master surfaces as a named error, not a printed value.

The derivation is a frozen v1 contract. Its parameters (SHA-256, the fixed salt
`tagwright.ballast.repo-key.v1`, the `service:<name>` info, 32 output bytes,
URL-safe base64) will not change in place, because changing any of them would
orphan every existing repository. If it ever must change, it ships as a new,
separately named derivation that repositories are migrated to on purpose. The
recipe is published in [RECOVERY.md](RECOVERY.md) so a repository is recoverable
by hand with nothing but a standard library, even if the Ballast binary is gone.

## The Docker socket

Ballast is more than a socket reader, and it is honest about that. It uses the
socket to discover and inspect services, and for a consistent backup it also
stops a container before the snapshot and starts it again after, and it execs
into a container to run a database dump straight into `restic --stdin`. Stop,
start, and exec all go through the socket, so Ballast's socket access is not
read-only in the way a pure discovery tool's would be.

Access to the container socket is effectively root on the host. A component that
can stop, start, and exec into containers can reach a great deal, and Ballast is
one such component by necessity: taking a consistent backup of a running database
requires it. Treat the Ballast container and its socket access as sensitive
accordingly. The state directory, the volumes root it reads to back things up,
and the config and secrets it reads are otherwise mounted read-only.

## What Ballast never stores, and what it does write

Ballast writes JSON run, verify, and check records for every backup, plus
backup-time manifests and a stable host identity, all under its state directory.
Those records describe what happened: snapshot IDs, times, paths, and outcomes.
They do not contain repository passwords, the master key, or object-storage
credentials. The record format is documented in [RECORDS.md](RECORDS.md) for
anyone parsing it downstream, and the thing to know for security is that it is
safe to parse without exposing a secret.

## Recovery, and the loss you cannot recover from

- **Losing the master key is unrecoverable.** restic repositories are encrypted
  with scrypt-derived keys from the derived password, and a lost master means
  every derived password is gone with it. There is no support path, no backup
  service, and no amount of compute that gets a repository back once the master
  that derived its password is gone.
- **Back up the master and the age key off-host, separately.** Back up the
  SOPS-encrypted master file itself, off the host, and back up the age key that
  decrypts it, off the host, kept apart from it. An age key that only ever lived
  on the failed machine leaves you holding ciphertext you can no longer open, and
  the encrypted master you saved elsewhere is then just as lost as if you had
  saved nothing.
- **Renaming a service moves its repository.** The derivation is bound to the
  service name through the HKDF info parameter, so a rename derives a different
  password and points at what restic sees as a different, unreachable repository.
  Pin the original name with `ballast.name` if you rename a service but want to
  keep its history.
- **Recovering a password needs only the master and the name.** `ballast key
  <service>` prints a repository password and nothing else, with no Docker socket
  and no running container required, which is the situation a disaster recovery
  is usually in. [RECOVERY.md](RECOVERY.md) walks the full path, including the
  by-hand derivation for when the binary itself is unavailable.

## Reporting a vulnerability

Report a suspected vulnerability through GitHub's private vulnerability reporting
on this repository: open the Security tab and choose "Report a vulnerability".
That opens a private advisory visible only to the maintainers, which keeps the
report out of public issues while it is being worked. Fixes are coordinated
there, and public disclosure follows a fix rather than preceding it. The Security
tab is the channel for this.
