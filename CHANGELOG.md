# Changelog

All notable changes are listed here, newest first ([Keep a Changelog](https://keepachangelog.com),
[Semantic Versioning](https://semver.org)).

## [Unreleased]

First release.

### Repositories
- karga is split into three repositories with their history: karga-server (this one), the karga
  CLI (`github.com/hasankaradmir/karga`) and karga-sdk, the module both use. Each has its own
  install script; this one installs karga-server as a systemd service, without credentials.

### Breaking (compared with earlier builds)
- Workloads no longer expose ports: `hostPort`, `hostIP` and `routes` are gone from the Workload
  (a clear error points to Services). Ports are named, and a `kind: Service` gives a workload a name
  inside its release and exposes it on gateway listeners.
- The gateway's listen addresses and listeners belong to the host's administrator: `karga gateway
  listener add|remove|list`, `karga gateway set listen` (default `127.0.0.1`). Whoever used
  `hostIP: 0.0.0.0` sets `karga gateway set listen 0.0.0.0`.
- karga-server owns the state and the runtime; the karga CLI is its client on
  `/run/karga/karga.sock` and stops with `karga-server is not running` without it (`create`,
  `lint`, `template`, `show` work alone). The service runs as the `karga` user with its state in
  `/var/lib/karga`; install with `KARGA_SYSTEMD=1`. The CLI's `--home` is gone (`--socket` instead).
- `dependsOn` on a workload: it starts only once the Services it names are ready behind the release
  proxy; a failed dependency keeps it from starting.
- karga-server repairs releases on its own (Docker events and every 30 seconds): removed or
  hand-changed containers are recreated, stopped ones started, unhealthy ones restarted, without a
  new revision. `karga reconcile`, `karga pause|resume` and `karga events`; `karga status` shows the
  last repair and what it gave up on.
- Operations go on when the CLI is interrupted (Ctrl-C, a lost connection); the CLI says how to
  follow them. A karga-server that is killed mid-operation finishes it when it starts again
  (`resume` in `karga events`).
- The CLI and karga-server check that they speak the same API version (`karga version` shows
  it). Operations record the user of the CLI that asked for them (`USER` in `karga events`).
- Linux only: without karga's gateway and release proxy (macOS, a remote Docker host) a plan with
  Services is refused. Upgrading an installed release with its new package recreates its workloads
  once.

### Packages and releases
- Packages: Go templates with Sprig, `values.yaml` and JSON Schema (writeOnly values are never
  stored), `karga create` to start one, `lint`, `template`, notes (`NOTES.txt`).
- Resources: Workload, Service, Volume, Network, Config, Secret.
- Releases: install, upgrade (`--reuse-values`, `--install`), rollback, uninstall; immutable
  revisions in SQLite; `history`, `diff`, `status --watch`, `get values|manifest|notes`.
- Plans as a summary (`--verbose` for fields) and live progress while applying.
- An upgrade recreates containers, networks and volumes removed outside karga; an install that
  fails before creating anything leaves no release behind.
- Data in an image's `VOLUME` path that no Volume covers survives upgrades (the anonymous volume
  moves to the new container), with a warning to mount a Volume there.
- `stopTimeout` on a workload: how long it gets to shut down before it is killed (default 10s);
  the Postgres example uses 60s.
- An upgrade or rollback that would delete a `retention: delete` volume stops unless given
  `--delete-volumes`; the plan marks what happens to a removed volume's data.

### Services and upgrades without downtime
- A release proxy (Envoy) serves the release's Services; `updateStrategy: replace` starts the new
  container next to the old one and switches the proxy over, so no client loses a request.
- A host gateway (Envoy) serves the administrator's listeners: tcp, and HTTP(S) by host and path,
  with TLS from a Secret and redirects from port 80.
- `tcp`/`http` health checks need no tool in the image.

### Operations
- `karga logs`, `karga exec`, `karga gateway status`, `karga state backup`,
  `karga volume backup|restore`, shell completion.
- Memory (without extra swap), CPU and process limits; OOM kills and restarts in `status`;
  container log rotation; `no-new-privileges` by default and a `security` block.
