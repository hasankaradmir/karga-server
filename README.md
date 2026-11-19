# karga-server

The server of [karga](https://github.com/hasankaradmir/karga-cli), package management for containers
on a **single host** without Kubernetes. karga-server owns the state (YAML files), talks to Docker,
runs installs, upgrades and rollbacks, and keeps every release running as declared: it repairs
removed, stopped, unhealthy or hand-changed containers. The **karga** CLI is its client on a local
unix socket; both use the API of [karga-sdk](https://github.com/hasankaradmir/karga-sdk).

How to use karga (packages, resources, commands): the CLI's
[README](https://github.com/hasankaradmir/karga-cli#readme) and
[USAGE.md](https://github.com/hasankaradmir/karga-cli/blob/master/USAGE.md). This repository is about
running the server.

## Requirements

- Linux with a local, rootful Docker Engine (not rootless) and systemd (amd64 or arm64). karga-server connects through
  `DOCKER_HOST` or the default Docker socket.
- Go 1.27 to build.

## Install

From a release, checked against the release checksums:

```bash
curl -fsSL https://raw.githubusercontent.com/hasankaradmir/karga-server/master/scripts/install.sh | sh
sudo usermod -aG karga $USER      # then log in again: members of the karga group may use karga
# KARGA_VERSION=v0.1.0 picks a version, KARGA_BIN_DIR another directory (default /usr/local/bin),
# KARGA_SYSTEMD=0 installs the binary only
```

The script installs `karga-server` as a systemd service: a `karga` system user in the `docker`
group runs it, its state is `/var/lib/karga`, and it listens on `/run/karga/karga.sock` (mode
0660, group `karga`). Then install the CLI:
[karga](https://github.com/hasankaradmir/karga-cli#install).

From source, without a published release: `make dist` builds the release archives and
`checksums.txt` in `dist/` and prints the line that installs them with this script
(`KARGA_DOWNLOAD_URL=file://…`, `KARGA_SKIP_SIGNATURE=1`). `make build` alone produces
`bin/karga-server`; the unit file is `deploy/systemd/karga-server.service`.

```console
$ systemctl status karga-server
$ journalctl -u karga-server -f          # the server's log, one line per operation with its user
$ karga version                          # the CLI's and the server's version and API version
```

## Running it

- **Flags:** `--home` (or `KARGA_HOME`; default `$XDG_DATA_HOME/karga` or `~/.local/share/karga`,
  `/var/lib/karga` for the service), `--socket` (or `KARGA_SOCKET`; default
  `/run/karga/karga.sock`), `--reconcile-interval` (default 30s; `0` repairs only on container
  events), `--allow-capabilities` (see below), `--shutdown-grace` (default 5m: how long running
  operations may take to finish on stop; keep the unit's `TimeoutStopSec` above it), `--edge-subnet`
  (or `KARGA_EDGE_SUBNET`; default `10.213.0.0/16`: the range of the `karga-edge` network the
  gateway reaches release proxies on; pick one no other network of the host uses; it cannot change
  while proxies use it), `--version`. It logs text to stderr (the journal),
  with every secret value it knows masked. One server owns a `KARGA_HOME`.
- **Compatibility:** the CLI and the server work together when they speak the same major API
  version (`2.x`), whatever their release versions; otherwise every command says which one to
  upgrade.
- **Operations** run in the server: a client that goes away (Ctrl-C) does not stop them. Operations
  on one release run one after the other, from planning to the end. On `systemctl stop` the server
  waits up to `--shutdown-grace` (5 minutes) for running operations (the unit's `TimeoutStopSec` allows it). Killed in
  the middle of one, it finishes the operation when it starts again (`resume` in `karga events`);
  commands on that release wait for it, the others are served at once, and a Docker that is not up
  yet is waited for. Every operation records the user of the CLI that asked for it, as
  the kernel reports it on the socket.
- **Capabilities:** a package may add only the Linux capabilities Docker gives containers by
  default (`CHOWN`, `NET_BIND_SERVICE`, `SETUID`, ...; usually after `capDrop: [ALL]`). Others give
  a container power over the host and are refused unless the administrator allows them:
  `--allow-capabilities NET_ADMIN,SYS_PTRACE` (`ALL` allows any).
- **Repairs:** on Docker events and every reconcile interval the server brings each release back to
  its current revision, without a new revision; `karga pause <release>` stops that for maintenance.

## Traffic through karga's Envoys

karga runs two kinds of Envoy (Envoy 1.36.13 distroless, pinned by digest, about 150MB, one worker
thread, about 15MB RAM each, limited to 128Mi, 0.5 CPU and 256 processes). Envoy updates ship with
karga releases; a new image recreates the gateway and the release proxies on the next apply, a short
interruption. `KARGA_PROXY_IMAGE` overrides the image.

- **Release proxy** (`<release>-karga-proxy-<hash>`, one per release with Services): answers to the release's
  Service names on its networks and forwards each Service port to the current container of its
  workload. Workloads carry only a per-revision alias meant for the proxy. It publishes
  nothing: the gateway reaches it on the edge network (`karga-edge`, D42) at its release's
  address, on the Service ports themselves.
- **Gateway** (`karga-gateway-<hash>`, one per host, host network): the host's only entry point. Its
  settings belong to the host's administrator, not to packages: the addresses it listens on
  (`karga gateway set listen`, default `127.0.0.1`) and its **listeners** (`karga gateway listener
  add`): a name, a port and a protocol. A new host has `http` (80, with `redirectHTTPS`) and
  `https` (443). A Service exposes a port on a listener by name; exposing on a listener that does not exist,
  of another protocol, or that another release holds is refused before anything changes. HTTP and
  HTTPS listeners select a Service by `Host` header and path (`/x/*` by segment, `/*`, or exact; `to`
  replaces a prefix in what the Service gets), so several releases share them;
  the backend receives the client address in `x-forwarded-for`. A tcp listener belongs to one Service.
  Listeners nobody uses are not opened. karga creates the gateway when a Service is exposed and
  removes it with the last one. `karga gateway status` shows whether it is ready and what it serves.
  `karga gateway set access-log on` logs every request (see `karga logs --gateway`).

A replace switches the release proxy, which every client goes through: no request is lost, from
outside or from other workloads. Notes:

- Adding or removing a Service reconnects the release proxy to its networks, and exposing a new port
  recreates it; both briefly interrupt proxied connections. The plan says so.
- **TLS:** an exposure on an https listener names a Secret with the PEM certificate chain as
  `tls.crt` and the key as `tls.key`. The certificate is chosen by SNI, so several hosts and releases
  share the listener. Plain HTTP on an http listener with `redirectHTTPS` (the default `http` has it)
  redirects to HTTPS, unless it serves that host itself. A new certificate is a new Secret value; the gateway serves it after the upgrade. One
  host has one certificate. There is no automatic certificate (ACME) yet.
- Services carry TCP only; there is no UDP.
- The gateway runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE` (to bind low ports in the
  host namespace) and `no-new-privileges`. It is a single point for outside traffic; it restarts
  automatically.
- Local users can reach the proxies' edge addresses (`10.213.x.y`) directly, bypassing the
  gateway. Containers on the edge network cannot reach each other.
- A connection open during a replace stays on the old container until it closes or `drainTimeout`
  passes. Configuration is validated before it is written; if Envoy still rejects it (for example a
  port it cannot bind), the error shows Envoy's reason.
- After a reboot or Docker restart the gateway, proxies and workloads come back on their own; a
  proxy keeps its edge address.

## State

Everything karga knows is in YAML files under `$KARGA_HOME/state` (directories `0700`, files
`0600`): a directory per release with its revisions and applied resources, and each release's
operations (ADR-0017). Only karga-server writes them; proxy and gateway configuration are rebuilt
from them and from Docker. To back up, stop karga-server and copy `$KARGA_HOME` (the state, the
`state.key`, the gateway settings in `gateway-settings/` and the revisions' Config and Secret files); restore by copying it back.
karga-server refuses state written by a newer version.

`karga volume backup <release> <volume> <file.tar.gz>` copies a volume's files through a workload
that mounts it (no helper container). It runs live by default; `--stop` stops the workloads
mounting the volume for a consistent copy (for databases prefer it, or the database's own dump).
`karga volume restore` stops them, writes the files back (overwriting files of the same name,
keeping others) and starts them again.

`$KARGA_HOME/state.key` keys the database's digests of secret values. Back it up with the database
if you can keep both safe; restored without it, karga creates a new key and applies each release's
Secrets (and what mounts them) once more on its next upgrade.

## Security notes

- Access to the Docker socket is root-equivalent on the host, and so is access to karga's socket:
  add to the `karga` group only users you trust with it. The socket is local only; there is no
  remote API. karga-server runs as the unprivileged `karga` user (in the `docker` group), with
  `NoNewPrivileges`, a read-only system and no access to home directories (packages and values files
  travel from the CLI over the socket).
- Packages cannot open ports: workloads publish nothing, and only the gateway's listeners, which the
  host's administrator defines, are reachable from outside, on `127.0.0.1` unless `karga gateway set
  listen` says otherwise.
- Containers are never privileged and never join the host PID, network or IPC namespaces, except the
  gateway, which uses the host network (see above). Release proxies run as the karga user. Proxy and
  gateway configuration and admin sockets are in `0700` directories under `$KARGA_HOME/proxies/` and
  `$KARGA_HOME/gateway/`.
- Config and Secret files live under `$KARGA_HOME/releases/<release>/<revision>/`. That directory is
  `0700`; the files inside are `0644` so any user in the container can read them. The state files
  (`$KARGA_HOME/state`) are `0600` in `0700` directories.
- Secret values never appear in output, logs or the state. The state keeps HMAC-SHA256
  digests of them (keyed by `$KARGA_HOME/state.key`, `0600`) to detect changes; without the key they
  cannot be used to guess a password. Releases written by an older karga re-apply their Secrets once.
- TLS keys are copied into `$KARGA_HOME/gateway/certs/` (`0600`, in a `0700` directory) for the
  gateway.
- Image pulls are anonymous; private registries are not supported yet.
- A variable a workload takes from a Secret (`environmentFrom`) is stored only as a reference, but
  its value is in the container's environment, which `docker inspect` shows; plans warn about it.

## Development

```bash
make test               # unit tests
make lint               # golangci-lint
make test-integration   # Linux with Docker: adapter contract and deployments
make test-e2e           # Linux with Docker: builds karga-server and the CLI, runs the lifecycle
make test-linux         # both in a Linux VM (OrbStack or Lima; KARGA_VM names it, default karga-linux)
```

The e2e tests drive the karga CLI: `KARGA_CLI` names a binary, otherwise it is built from
`KARGA_CLI_SRC` (default: a checkout of [karga](https://github.com/hasankaradmir/karga-cli) next to
this one). Integration and e2e tests run on Linux only (they stop with a message elsewhere).
`make test-linux` creates and provisions the VM on first use and runs `TARGETS` there (default
`test-integration test-e2e`), with the karga and karga-sdk checkouts next to this one copied
along. Integration tests run in parallel and share one `KARGA_HOME`.
`KARGA_TEST_DAEMON_RESTART=1 make test-integration` additionally restarts the Docker daemon (needs
passwordless sudo).

Types shared with the CLI (requests, results, plans, the package format) live in karga-sdk: change
them there first, then require the new karga-sdk here.

Releases: add the version to `CHANGELOG.md`, then `git tag vX.Y.Z && git push --tags`; the release
workflow builds the binaries with GoReleaser and creates a draft GitHub release with the changelog
section as notes; publish it to make it the latest release.

Documentation for working on karga: [docs/](docs/README.md) (design, roadmap, architecture
decisions).

## License

Apache License 2.0; see [LICENSE](LICENSE). Security issues: see [SECURITY.md](SECURITY.md).
