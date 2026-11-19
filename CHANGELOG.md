# Changelog

All notable changes are listed here, newest first ([Keep a Changelog](https://keepachangelog.com),
[Semantic Versioning](https://semver.org)).

## [Unreleased]

### Changed
- A workload's container is named `<release>-<workload>-<6 hex of its digest>`, like a Kubernetes
  pod-template-hash: the same spec keeps its name, a changed one gets a new name (D34). A replace
  starts the new container under its own name next to the old one and no longer renames it
  (`<name>-next` is gone); an interrupted replace keeps the container the release proxy routes to.
  Reach a workload through its Service: its container name changes with every update.
- `karga status` reports a release unhealthy (exit 6) when one of its Services is routed to no
  backend. The roadmap lists automatic rollback and default resource limits as not planned.
- karga-server refuses an uninstall, a volume restore, removing a gateway listener, listening on
  an address other than loopback, and an upgrade or rollback deleting volumes unless the client
  says the user agreed: another client of the API cannot skip `karga`'s question.
- API 3.0 (karga-sdk): karga CLIs of API 2 are refused with a message to upgrade them. Every
  request is checked before it reaches the service: a release name must be a DNS label that does
  not start with `karga`, for every operation (a name like `../x` could reach a path outside the
  state before). Errors of logs, volume backups and exec keep their kind.
- A package's digest is its content (karga-sdk): the same package sent by CLIs built with other
  Go versions, or as a directory and as a `.tgz`, is recognised as the same (`install` is a no-op
  instead of a conflict). Releases installed before get a new revision on their next install or
  upgrade.
- `--set` and `--set-string` apply in the order they were given, and `--set a[1]=x` edits the
  default list instead of replacing it with `[null, x]` (ADR-0018).
- Plans warn about every variable a workload takes from a Secret (`environmentFrom`): anyone who
  can inspect the container sees its value. Packages may use only `bridge` networks and `local`
  volumes (karga-sdk); other drivers could reach the host's network past the gateway.
- A package cannot keep karga-server rendering forever or exhaust its memory: template loops are
  limited to 1,000,000 iterations per render and the time limit is checked as they run
  (karga-sdk).
- `history` shows deployed revisions a later deployment replaced as `superseded`; only the one
  that runs is `deployed`. The state is unchanged, and rollback targets are the same.

### Added
- `make dist` builds the release archives and `checksums.txt` without a release; `install.sh`
  installs them from `file://`. With `KARGA_SKIP_SIGNATURE=1` it no longer suggests cosign.
- Workloads take variables from Secrets (`environmentFrom`): the state keeps the reference, the
  value is set when the container is created, and a new value recreates the container.

### Fixed
- A container name the plan would create that another release, or something else than karga,
  holds failed the deployment half way. It is refused before anything changes, naming the release
  that holds the container.
- A rollback of an uninstalled release recorded an operation before it failed; it is refused
  first, pointing at `karga install`. karga-server warns at start when `--allow-capabilities` is
  `ALL`.
- Two karga-servers with different `KARGA_HOME`s on one Docker replaced each other's gateway for ever,
  and one with nothing to expose removed the other's. The gateway of another home is left alone now:
  exposing fails with a conflict naming that home. Moving to another home, remove `karga-gateway`
  with Docker first.
- `volume backup --stop` kept the workloads stopped and the release held for as long as the client
  took to read the backup; the copy now goes to a temporary file under `$KARGA_HOME/tmp` (as large
  as the backup), the workloads run again, and the file is sent after.
- Waiting for the gateway's lock (another release changing what it exposes) could not be
  interrupted; it ends with the operation's context now.
- Revisions were pruned to `--history-max` only after a deployment that succeeded: an upgrade
  failing again and again kept every revision and its files. Failed deployments prune too. Temporary
  files a crash left in the state are removed when karga-server starts.
- A repair waited up to 5 minutes for a container that did not come up, holding every other
  release's repairs that long, and the reconciler's queue dropped releases beyond 256. Repairs wait
  at most a minute and are tried again with growing pauses; the queue keeps every release once
  (ADR-0019).
- A secret quoted in an error message (a failed connection with its password, say) reached the
  client and the operations in the state as it was; the redactor masks those too now. A first
  install that failed left its Config and Secret files on disk; they are removed with it.
- The gateway's and the proxies' files (settings, routes, certificates, Envoy's configuration) were
  replaced without being synced, through a fixed temporary name: a power loss could leave Envoy an
  empty file. Every file karga-server writes now goes through one helper that syncs the file and its
  directory; the state syncs its directory after a removal too.
- karga-server answered no request until it had resumed every operation it left running, each up to
  its timeout, and a resume that met Docker not up yet failed and left its release interrupted for
  good. It now serves at once: commands on a release being resumed wait for it, and a resume waits
  for Docker to answer, with growing pauses.
- After a reboot, an operation an earlier karga-server left running could keep its release locked
  for good (`ErrLocked`, even with `--force`) when another process got its PID. Only one
  karga-server runs per home, so such an operation is interrupted, and resumed or aborted as any.
- A command waiting for a busy release went on waiting after its client went away (Ctrl-C), and ran
  later; it stops waiting now, as ADR-0015 says. What started still runs to the end.
- The state store refuses a release name that is not a DNS label itself, as a second line behind
  the API's check: names build its paths, so `../x` reached outside the state.
- writeOnly values under a map or a list of the values schema (`additionalProperties`, `items`, …)
  were stored as written in the revision: the masking did not follow `*` in secret paths. They are
  stored as `<redacted>` now, and `--reuse-values` asks for them again.
- The warnings of a deployment's run (from the gateway) were dropped when it succeeded.
- After a restart the server did not mask Secret values in its log until each release was
  applied again; it now registers the current revisions' values when it starts.
- The gateway cut every HTTP request after 15 seconds (Envoy's default route timeout) and refused
  WebSocket upgrades. Requests now run until done, upgrades pass through, and a stream idle for
  5 minutes is closed.

## [0.1.0] - 2026-11-18

First release.

### Repositories
- karga is split into three repositories with their history: karga-server (this one), the karga
  CLI (`github.com/hasankaradmir/karga-cli`) and karga-sdk, the module both use. Each has its own
  install script; this one installs karga-server as a systemd service, without credentials.

### Release
- Built with Go 1.27.4 (net/http fix GO-2026-6859) and golang.org/x/net 0.60.0 (GO-2026-6603, GO-2026-6610, GO-2026-6611, GO-2026-6612, GO-2026-6617); CI runs govulncheck 1.10.0.
- Releases carry an SBOM per archive and a keyless (Sigstore) signature of `checksums.txt`;
  `install.sh` verifies it when `cosign` is installed (`KARGA_SKIP_SIGNATURE=1` skips it).
- The release workflow runs the tests, the integration tests and the linter before it publishes; CI runs `govulncheck`, and
  every GitHub Action is pinned by commit. The e2e job tests the CLI tag in the repository variable
  `KARGA_CLI_REF` (master without it).

### Fixed
- Two karga-servers with different `KARGA_HOME`s on one Docker replaced each other's gateway for ever,
  and one with nothing to expose removed the other's. The gateway of another home is left alone now:
  exposing fails with a conflict naming that home. Moving to another home, remove `karga-gateway`
  with Docker first.
- A repair waited up to 5 minutes for a container that did not come up, holding every other
  release's repairs that long, and the reconciler's queue dropped releases beyond 256. Repairs wait
  at most a minute and are tried again with growing pauses; the queue keeps every release once
  (ADR-0019).
- A secret quoted in an error message (a failed connection with its password, say) reached the
  client and the operations in the state as it was; the redactor masks those too now. A first
  install that failed left its Config and Secret files on disk; they are removed with it.
- karga-server answered no request until it had resumed every operation it left running, each up to
  its timeout, and a resume that met Docker not up yet failed and left its release interrupted for
  good. It now serves at once: commands on a release being resumed wait for it, and a resume waits
  for Docker to answer, with growing pauses.
- After a reboot, an operation an earlier karga-server left running could keep its release locked
  for good (`ErrLocked`, even with `--force`) when another process got its PID. Only one
  karga-server runs per home, so such an operation is interrupted, and resumed or aborted as any.
- A command waiting for a busy release went on waiting after its client went away (Ctrl-C), and ran
  later; it stops waiting now, as ADR-0015 says. What started still runs to the end.
- Repairing or resuming a release rewrote its Config and Secret directories from scratch: running
  containers were left with an empty, deleted directory. They are now updated in place.
- Two commands on one release could plan against the same state and take the same revision; the
  reconciler could repair from a revision an upgrade had just replaced. An operation now holds its
  release from reading its state to the end, and waiting for it can be given up.
- The server logged secret values: the log redactor was never connected. It now masks every
  secret value the server knows.
- The server answered requests while it resumed interrupted operations; it now resumes first.
- Under systemd the server was killed after 90 s instead of letting operations finish for 5
  minutes (`TimeoutStopSec=330`). The unit drops every capability and adds sandboxing.
- `install.sh` restarted the server on every run, failed when the docker group was missing and
  did not add an existing karga user to it.
- Errors had status 400 whatever they were; they now travel with their kind (not found, conflict,
  invalid, internal).
- `karga uninstall --dry-run` shows what would be removed.
- Two-step state writes (a release's status with its new revision, a revision's outcome with its
  release, an uninstall's resources) are one transaction.
- Docker calls have deadlines (2 minutes, 30 for an image pull): a daemon that stops answering no
  longer holds a release for ever.
- A failed read of `state.key` is retried instead of failing every deploy until a restart, and the
  key is synced to disk when it is created.
- The gateway normalizes paths and merges slashes before routing by prefix, and takes a backend
  out after three failed checks instead of one.
- Repairs no longer update a release proxy (after a server upgrade, for example); they only create
  what is missing.
- The Envoy admin client no longer keeps a connection pool per proxy.
- An interrupted operation resumes with the options its request gave (`--delete-volumes`,
  `--keep-history`, `--strategy`, `--timeout`): before, a resume assumed volumes could be deleted
  and an uninstall kept the history.
- What a release exposes is checked again under the gateway lock just before it is written: two
  releases deploying at once can no longer take the same listener, host or path. A gateway port
  karga-server cannot test (below 1024, without privileges) gives a warning instead of passing
  silently.
- A live `karga volume backup` no longer holds the release (a slow client blocked deployments); a
  `--stop` backup still does, as an operation.
- Rootless Docker does not get karga's gateway and release proxy (their host ports and probes need a
  rootful daemon); Services there are refused with a clear message.
- karga's Envoys have an overload manager: they shed load at 100 MiB of heap and accept at most
  4096 connections, within their 128 MiB limit.
- `--shutdown-grace` sets how long running operations may take to finish on stop (5m).
- A repair waiting out its backoff is tried again when it may run, even if no Docker event follows
  (a container removed while the reconciler looked at it stayed down).

### Breaking (compared with earlier builds)
- The state is YAML files under `$KARGA_HOME/state` (ADR-0017) instead of a SQLite database, and
  nothing karga-server stores is JSON: `gateway.yaml`, `expose.yaml`, Envoy's `envoy.yaml`,
  `lds.yaml`, `cds.yaml`, the volume backup manifest. A `karga.db` of an earlier build is refused,
  not converted. `karga state backup` is gone: a backup is a copy of `$KARGA_HOME`. A release keeps
  its newest 200 operations.
- API 2.0 (karga-sdk): a CLI and a server work together when their major API version is the same.
  The state model (releases, revisions, operations) lives in karga-server and the CLI sees views of
  it. `--allow-capabilities`: packages may add only Docker's default capabilities unless the
  administrator allows more. A writeOnly value used outside a Secret is refused.
- Workloads no longer expose ports: `hostPort`, `hostIP` and `routes` are gone from the Workload
  (a clear error points to Services). Ports are named, and a `kind: Service` gives a workload a name
  inside its release and exposes it on gateway listeners.
- The gateway's listen addresses and listeners belong to the host's administrator: `karga gateway
  listener add|remove|list`, `karga gateway set listen` (default `127.0.0.1`). Whoever used
  `hostIP: 0.0.0.0` sets `karga gateway set listen 0.0.0.0`.
- karga-server owns the state and the runtime; the karga CLI is its client on
  `/run/karga/karga.sock` and stops with `karga-server is not running` without it (`create`,
  `lint`, `template`, `show` work alone). The service runs as the `karga` user with its state in
  `/var/lib/karga`; karga-server's install script sets up the service. The CLI's `--home` is gone (`--socket` instead).
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
  revisions in karga-server's state; `history`, `diff`, `status --watch`, `get values|manifest|notes`.
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
- `karga logs`, `karga exec`, `karga gateway status`,
  `karga volume backup|restore`, shell completion.
- Memory (without extra swap), CPU and process limits; OOM kills and restarts in `status`;
  container log rotation; `no-new-privileges` by default and a `security` block.
