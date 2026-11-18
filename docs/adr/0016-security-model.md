# ADR-0016: security model

**Status:** accepted

**Decision:**

- **One trust level.** Whoever can open the karga socket (mode 0660, group `karga`) administers the
  host: they can run any image with the release's volumes, read logs and exec into containers. This
  is the Docker socket's model; there are no users, roles or remote API. The server records the
  peer's user on every operation (`SO_PEERCRED`) for the audit trail, not for authorization.
  Forwarding the socket to another machine is not supported; use `ssh host karga …`.
- **Packages are less trusted than the administrator.** A package cannot run privileged
  containers, add capabilities beyond Docker's defaults (unless the administrator allows them with
  `--allow-capabilities`), publish host ports, or change the gateway's listeners. Rendering and
  archives are bounded in size, depth and time.
- **Secrets.** Secret values never appear in plans, output, logs (the server's redactor masks every
  value it knows) or the state: the state keeps only HMAC digests keyed by `$KARGA_HOME/state.key`.
  `writeOnly` is allowed only in Secrets, and those values are stored as `<redacted>`.
- **Secret files are plain files** under the revision's directory in `$KARGA_HOME` (directories
  0700, owned by the server's user), mounted read-only. Anyone who can read `$KARGA_HOME` or run as
  the server's user reads them; there is no encryption at rest.

**Reason:** karga is a single-host tool for the host's administrator. A finer model (users,
per-release rights) would need authentication and still be bypassed by anyone in the `docker`
group. Encrypting secrets at rest would need a key the server can read, on the same disk.

**Consequences:** adding someone to the `karga` group is giving them root on the host. Host disk
backups contain the secrets in clear; protect them like the host.
