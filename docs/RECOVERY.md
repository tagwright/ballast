# Recovery

This document is for the day something has gone wrong: the host is gone, the
compose stack is gone, or you just need a repository password and Ballast
is not running. Read it before you need it.

## How repository passwords are derived

Ballast does not store a password per repository. It stores exactly one
secret, the master key, and derives every service's restic repository
password from it with HKDF-SHA256, keyed on the service name. The derivation
is deterministic: the same master and the same service name always produce
the same password, every time, on any machine.

That has one consequence worth stating plainly: the master key plus the
service name is enough to recover a repository's password. No per-service
state, no database, no file Ballast wrote anywhere, is needed. If you have
the master and you know what the service was called, you can always get back
in.

## Generating the master

Generate the master with:

```
openssl rand -base64 32
```

Store the output as the SOPS secret named `repo-master-key`. It must be a
single line with no surrounding whitespace: Ballast trims leading and
trailing whitespace on read, but the value SOPS decrypts to disk (or the
environment variable it falls back to) should already be exactly one line,
with nothing else in the file.

Do not reuse a master key across installations you want to be able to
recover independently, and do not derive it from anything memorable. Let
`openssl rand` generate it.

## The catastrophic-loss property

Losing the master key is unrecoverable. There is no brute-force path around
it: restic repositories are encrypted with scrypt-derived keys from a
256-bit password, and a lost master means every derived password is gone
with it. There is no support request, no backup service, no amount of
compute that gets a repository back once the master that derived its
password is gone.

This means the master key is the single most important thing Ballast
touches, more important than any individual repository, because it is what
every repository ultimately depends on. Treat it accordingly:

- Back up the SOPS-encrypted master key file itself, off the host.
- Back up the age key that decrypts it, off the host, separately.

Both matter. An on-host-only age key dies with the host exactly as surely as
losing the master itself would: if the age key only ever lived on the
machine that failed, the SOPS-encrypted master you backed up elsewhere is
just ciphertext you can no longer open. Keep both copies somewhere that
survives the host being gone entirely, and keep them in a place you'll
actually remember to look during a real recovery.

## The rename caveat

A service's derived password is bound to its name: the HKDF info parameter
is `service:<name>`, so a different name derives a different password, and
therefore points at what restic sees as a different (and, to your existing
repository, unreachable) repository.

If you rename a service, its old repository is orphaned under the old
derivation unless you carry the old name forward on purpose. If you need to
rename a service but keep its backup history, pin the original name with the
`ballast.name` label rather than relying on the compose service name or
container name, so the identity used for derivation stays fixed across the
rename.

## Normal recovery

With Ballast still available, recovering a repository password is one
command:

```
ballast key <service>
```

This prints the service's repository password to stdout and nothing else.
It needs only the master secret, resolved the same way the daemon resolves
it, and works with no Docker socket and no running container, which is
exactly the situation a disaster-recovery run is usually in.

Use the printed password with restic directly:

```
export RESTIC_PASSWORD_FILE=/path/to/password_file
# or: export RESTIC_PASSWORD=<the printed value>
restic -r <repo> snapshots
```

Avoid leaving the password in shell history or an unencrypted log. Prefer
piping it straight into `RESTIC_PASSWORD_FILE` or a password manager over
pasting it somewhere it will linger.

## Listing and restoring snapshots

Reaching for `ballast key` plus raw `restic` commands is rarely the first
move. If the Ballast binary and its config are both available, `ballast
snapshots` and `ballast restore` are the normal way to list and restore a
service's repository, and they resolve the repository automatically instead
of making you assemble a restic URL and password by hand.

```
ballast snapshots <service>
ballast restore <service> --target /path/to/restore/into
```

Both commands resolve the repository the same way: if `<service>`'s
container is currently discoverable via its `ballast.*` labels, its own spec
builds the repository, exactly as a scheduled backup would. That is the
common case: the container that owns the repository is just stopped or
misbehaving, not gone.

For the actual disaster-recovery case, where the container (and its labels)
no longer exist at all, both commands fall back to explicit flags instead of
failing:

```
ballast snapshots <service> --destination <name> [--repo-path <path>]
ballast restore <service> --destination <name> [--repo-path <path>] --target /path/to/restore/into
```

`--destination` names a destination from `ballast.yml` directly (what
`ballast.repo` would otherwise have selected); `--repo-path` overrides the
sub-path within it if it differs from the service name. `ballast restore`
also takes `--snapshot <id>` (default `latest`) and repeatable `--include
<pattern>` to restore a subset of paths.

Only reach for the raw-restic recipe below when the `ballast` binary itself
is unavailable, or the config it needs isn't recoverable, either of which
`ballast key` (see above) plus this section's derivation walks you through.

## Recovery without Ballast

If the Ballast binary itself is unavailable, the derivation is simple enough
to reproduce by hand with nothing but a standard library. This is the v1
construction, frozen: it will not change, and any future version bump ships
as a new, separately-named derivation rather than an edit to this one.

The construction:

- Hash: SHA-256
- IKM (secret): the master key's text bytes, used verbatim (not base64-decoded)
- Salt: `tagwright.ballast.repo-key.v1`
- Info: `service:<name>`
- Output length: 32 bytes
- Encoding: base64, URL-safe alphabet, no padding

Python (standard library only):

```python
import hashlib
import hmac
import base64

def derive_repo_password(master: str, service: str) -> str:
    ikm = master.encode("utf-8")
    salt = b"tagwright.ballast.repo-key.v1"
    info = ("service:" + service).encode("utf-8")

    prk = hmac.new(salt, ikm, hashlib.sha256).digest()

    okm = b""
    t = b""
    counter = 1
    while len(okm) < 32:
        t = hmac.new(prk, t + info + bytes([counter]), hashlib.sha256).digest()
        okm += t
        counter += 1
    okm = okm[:32]

    return base64.urlsafe_b64encode(okm).rstrip(b"=").decode("ascii")

# derive_repo_password("<master key text>", "<service name>")
```

Go (standard library plus `crypto/hkdf`, Go 1.24+):

```go
package main

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

func derivedRepoPassword(master, service string) (string, error) {
	info := "service:" + service
	key, err := hkdf.Key(sha256.New, []byte(master), []byte("tagwright.ballast.repo-key.v1"), info, 32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}
```

Both snippets take the master key's raw text (not base64-decoded) and a
service name, and produce the same restic repository password `ballast key`
would. This is the entire recovery story once the tool itself is gone: the
master key, the service name, and this recipe.

## See also

- [README](../README.md), for how Ballast resolves secrets in normal
  operation.
- Ballast Architecture, in the tagwright section of the wiki, for how the
  master key and per-service derivation fit into the rest of Ballast's
  design.
