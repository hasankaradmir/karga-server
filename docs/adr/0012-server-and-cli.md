# ADR-0012: karga-server and the karga CLI

**Status:** accepted; supersedes ADR-0007

**Decision:** karga is split into two programs on one host:

- **karga-server** owns the state (its `KARGA_HOME`, `/var/lib/karga` for the systemd
  service) and the container runtime, and serves the release operations on a local unix socket
  (`/run/karga/karga.sock`, HTTP and JSON). It runs as the unprivileged `karga` user in the `docker`
  group. One server per `KARGA_HOME` (a lock file enforces it).
- **karga** is a client. It sends packages (as archives) and values files (as content) over the
  socket, and never opens the state or Docker itself. Progress, logs and backups are streamed;
  `exec` upgrades the connection and carries its streams as frames. Commands that only read a local
  package (`create`, `lint`, `template`, `show`) need no server.
- Access is local only: the socket is mode 0660, group `karga`. There is no remote API and no
  authentication beyond file permissions.

- **Reconciliation.** The server brings the runtime back to each release's current revision after
  Docker events and every 30 seconds: missing or hand-changed objects are created again, stopped
  containers started, unhealthy ones restarted. It never creates a revision or touches a failed or
  paused release; repairs back off and give up after 5 failures in 10 minutes.

**Reason:** A process that is always there can keep the runtime as declared between commands
(reconciliation) and is the single owner of the state, instead of every CLI invocation opening it.
Keeping the API on a unix socket keeps the security model of the Docker socket and needs no
authentication, certificates or API versioning across hosts.

**Constraints:** The server is always running (about 50 MB resident, mostly the binary; it does
nothing between events and the 30-second passes). One karga-server per host: the gateway is host-wide. Access to the karga socket is
as powerful as access to Docker. The CLI and the server must speak the same API major version, checked on every
request (ADR-0014); `karga version` shows both.
