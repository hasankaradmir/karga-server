# karga design

This document describes how karga works today. Architecture decisions and their reasons are in
[`adr/`](adr/); the implementation decision log is [`adr/decisions.md`](adr/decisions.md); what is
planned and what is known to be missing is in [`roadmap.md`](roadmap.md).

## 1. Goals and non-goals

karga installs, configures, upgrades and rolls back containerized applications on **one host**,
from versioned packages, without Kubernetes and with as few resources as possible.

Goals:

- Packages: versioned, templated, configurable through values validated by a JSON Schema.
- Releases: every install, upgrade and rollback is an immutable revision with a reviewable plan.
- Upgrades without downtime where the workload allows it, and a clear plan where it does not.
- Small footprint: one small server, a CLI and ordinary containers; no cluster, no control plane.
- Runtime independence: the package format does not depend on Docker.

- Nothing reachable by accident: packages cannot open ports; the host's administrator decides what is
  reachable from outside.

Non-goals: multi-node orchestration, scheduling, horizontal scaling, Kubernetes compatibility, a
package marketplace, automatic database migration rollback, platforms other than Linux.

## 2. Architecture

karga is two programs (ADR-0012) in three repositories (ADR-0013): karga-server (this one), the
karga CLI, and karga-sdk, the Go module both use. **karga-server** owns the state and the container
runtime; it serves the release operations on a local unix socket (`/run/karga/karga.sock`, HTTP and
JSON, no remote API). The **karga** CLI is its client: it packs packages and values files and sends
them, and streams progress, logs, `exec` (an upgraded connection carrying frames) and backups. Only
`create`, `lint`, `template` and `show` work without the server. The API is karga-sdk's `api`: a
`Backend` interface that `internal/server`'s `Backend` implements on the server (mapping it to the
release service) and `api.Client` in the CLI, the HTTP handler and the client. Each endpoint takes
its own request type, which the handler validates (release names are DNS labels that do not start
with `karga`) before the backend sees it. Every request carries
the API version `Major.Minor` (`Karga-Api`); the server refuses another major, and a field or
endpoint it does not know, so the CLI and the server may be of different releases while the major
is the same (ADR-0014). The server reads each client's user from the socket (`SO_PEERCRED`) and records it on the
operations it starts.

**Operations and restarts.** Operations do not depend on the client's connection: a client that goes
away loses only the progress. Each operation holds its release from reading its state to recording
the outcome, so what it plans cannot go stale; others wait, and a reconciliation skips a busy
release. On SIGTERM the server stops accepting requests and waits up to `--shutdown-grace` (5 minutes) for running
operations. When it starts, it holds the releases it left an operation running on and resumes them
in the background before it repairs anything (section 5); a command on such a release waits for its
resume, the others are served at once.

**Reconciliation.** Desired state is each release's current revision and its applied resources;
only install, upgrade and rollback change it. karga-server's reconciler (`internal/application/
reconciler`, the queue and the loop; `releases.Lifecycle.Repair`, the repairs) brings the runtime back to it:

- *Triggers:* Docker events of karga's containers (die, stop, destroy, oom, unhealthy; 2 s debounce),
  a pass over every release every 30 s (`--reconcile-interval`), the server's start, and every
  install, upgrade or rollback. One release at a time, from a queue that holds each release once
  and drops none (ADR-0019).
- *Scope:* deployed releases that are not paused and have no operation running. A failed release is
  the user's (D2). Within the server a per-release mutex serializes commands and repairs: a command
  waits for a repair; a repair skips a busy release.
- *Repairs:* missing workloads, proxies and networks and containers whose `io.karga.digest`
  label differs from the applied digest (changed outside karga) are created from the current revision
  through the usual plan and executor, creates only, without a new revision; stopped containers are
  started; unhealthy ones (Docker's check, or karga's tcp/http check failing `retries` times) are
  restarted; the gateway is synced if something is exposed. A missing Volume is not created: its
  files are gone, and its workloads would start on nothing (D46). The repair fails saying so, and
  what is missing waits until `karga reconcile --empty-volumes` (`RepairEmptyVolumes`) or an
  upgrade creates it. A repair waits at most a minute for what
  it recreated, so one release that does not come up holds up the others no longer than that.
- *Backoff:* per resource, 10 s doubling to 5 min. A repair the resource needs again within 10
  minutes counted as failed (a crash loop), and the pause keeps growing until it stays up that long.
  After 5 failures in 10 minutes it gives up; it tries again once they are older than that, or at
  once when the user acts on the release or runs `karga reconcile`. A warning that repeats every
  pass (Docker down, a repair given up on) is logged once, and its end once.
- Every repair is an operation of type `reconcile`; `karga events` lists them.

```
cmd/karga-server                entry point
internal/server                 karga-server: state, runtime, socket, peer credentials; backend.go maps
                                the API (karga-sdk/api) to the release service and back
internal/application/releases   the release service, built once by New(Deps): one unexported core
                                (state, runtime, locks, deploy steps) shared by Lifecycle (install,
                                upgrade, rollback, uninstall, resume, repair), Reads (status, history,
                                operations), Containers (logs, exec), Volumes and Gateway
internal/application/deployment plan (diff, strategies), execute, release proxy, probes (deployment.Env,
                                made once by NewEnv with the state home and the digest key)
internal/application/gateway    the host gateway: settings, exposures, its Envoy
internal/application/proxyops   what both share: runtime names and removal, the proxy Host
internal/ports                  Runtime, StateStore, ProxyControl and HostNet; contract suites
internal/adapters/{docker,filestore,fakeruntime}
internal/infrastructure/{envoy,hostnet,logging,config}

karga-sdk: api (the wire contract: types, error kinds, version, Backend, client, HTTP handler),
packages (the package engine and metadata), resource (the resource kinds); values, templates and
archives are internal to it. The server keeps its own state model (internal/domain/release) and its
own request and result types; only internal/server knows the API and maps between them.
```

The SDK depends on nothing of the CLI or the server, and the server on nothing of the CLI,
enforced by `depguard` in each repository's `.golangci.yml`.
The application layer talks to the container runtime, the state, the proxies and the host's network
only through `ports` (`Runtime`, `StateStore`, `ProxyControl` with the proxy configuration model,
`HostNet` for health probes and port tests); the composition root (`internal/server`) picks the
adapters: Docker, the file store, Envoy and the host network, and passes them down (no package
holds mutable state of its own). `depguard` enforces it. Every
runtime adapter passes the same contract suite (`internal/ports/runtimetest`); the fake runtime used
by unit tests passes it too.

## 3. Packages

A package is a directory or `.tgz`:

```
Package.yaml          apiVersion karga.io/v1alpha1, name, version (semver), runtime.supported
values.yaml           defaults
values.schema.json    optional JSON Schema; "writeOnly": true marks secret values
templates/*.yaml      resources; templates/_*.tpl hold named templates
files/                read with .Files.Get / .Files.Glob
NOTES.txt             optional template shown after install and upgrade
README.md
```

Rendering uses Go `text/template` with Sprig. Functions that read the host (`env`, `expandenv`,
`getHostByName`) or are not deterministic (time, random, generated keys, `bcrypt`, ...) fail the
render, so the same package and values always give the same resources. The context is `.Values`,
`.Release.Name`, `.Release.Revision`, `.Package.Name`, `.Package.Version` and `.Files`.

Values are the package defaults overlaid with `-f` files in order, then `--set` and
`--set-string` (`--set a=null` deletes a default), then validated against the schema (`--strict`
rejects undeclared keys). The package format is alpha (`v1alpha1`): it may still change.

## 4. Resource model

| Kind | Purpose |
|------|---------|
| Workload | One container: image, command, environment, networks, named ports it listens on, mounts, health check, resources, logging, security, update and stop timeouts, the Services it depends on. It publishes nothing; its name is not a DNS name (Docker still resolves the container name on the release's networks). |
| Service | A stable name for one workload in the release: ports mapped to the workload's ports, and exposures on gateway listeners (tcp, or HTTP(S) by host and path). |
| Volume | A named Docker volume; `retention: retain` (default) keeps it across uninstalls. |
| Network | A bridge network; Services of the workloads on it are reachable by name on it. |
| Config | Files mounted read-only. |
| Secret | Values mounted read-only as files; the state keeps only keyed digests. |

`resource.Decode` applies defaults (1024 processes, log rotation 10m×3, `no-new-privileges`, health
check port and path, a Service's target port and path) and validates each resource; a Workload that
still uses the removed `hostPort`, `hostIP` or `routes` gets an error pointing to Services.
`ValidateSet` checks references across the set (mounted volumes, configs and secrets, networks, a
Service's workload and target ports, TLS secrets with `tls.crt` and `tls.key`) and that a Service
port number and a listener's host and path are used once per release. Specs karga stored itself are
read with `resource.DecodeStored`, which ignores fields an older karga wrote. Because defaults are written into the spec, a new
default shows up in the plan as a field change.

## 5. Releases and state

The state is YAML files under `$KARGA_HOME/state` (ADR-0017), mode 0600 in 0700 directories:

| File | Holds |
|------|-------|
| `releases/<r>/release.yaml` | name, package, status, current revision |
| `releases/<r>/revisions/<n>.yaml` | immutable: package digest, values (writeOnly redacted), rendered manifest (secret values as digests), notes, status |
| `releases/<r>/resources/<kind>_<name>.yaml` | what is applied now: runtime name, spec, digest |
| `operations/<r>.yaml` | the release's newest 200 operations with their steps, PID, hostname and user |

Files are replaced atomically; a transaction writes the release file last, so a crash leaves at
most unreferenced files. `version.yaml` holds the format version; a newer one is refused. A release
file that cannot be read, or lacks its name or status (cut short by a disk error or a hand edit),
stops only its release: it is listed as `unreadable` (its status says why), the reconciler leaves
it alone, and an operation reads and writes only its own release's file.

Every mutating command records an operation first. One karga-server runs per home and only it runs
operations, so an operation found running after a restart is interrupted: it is resumed, or aborted
only with `--force`. Operations do
not depend on their client's connection. When karga-server starts it resumes the operations it
left running, with the options recorded on the operation (`--delete-volumes`, `--strategy`,
`--timeout`, `--keep-history`): install, upgrade and rollback deploy their recorded revision again
(the plan skips what is done; no new revision), uninstall runs again, a volume restore runs again
from the backup it kept under `$KARGA_HOME/restores` (without taking its restore point again; if
the backup is gone, the release is paused), others are aborted (ADR-0015). The finishing operation is recorded as `resume`. A deployment stopped before its revision was
recorded changed nothing, and its release keeps its status (a failed one stays failed); a resumed
deployment prunes by the request's `--history-max`. An operation left running on a release that is
gone (an uninstall stopped after it removed it) is closed. An uninstall removes the release's
containers, networks and volumes first, and its files and edge addresses only once they are gone,
so one that fails can be run again; a gateway that cannot drop its routes is a warning. A failed apply
marks the revision failed; an install that failed before creating anything removes the release; if nothing that was running changed (an image that cannot be pulled, an
unhealthy new container in a replace), the release keeps its status. Rollback re-applies a stored
revision as a new revision. `--history-max` (default 10) prunes old revisions.

Digests of secret material (Secret values, values, manifests) are HMAC-SHA256 keyed by
`$KARGA_HOME/state.key` (32 random bytes, created on first use), so a weak password cannot be
guessed offline from the state files.

## 6. Planning

`deployment.Diff` compares the desired resources with the applied ones by digest. Applied
workloads, networks and volumes that no longer exist in the runtime are left out first, so the plan
creates them again (removed by hand, or a state restored on another host). A workload's
digest covers its spec, whether a Service targets it and the content of the Configs and Secrets it
mounts, so changing a config file re-applies the workloads using it; a Service's digest covers the
Secrets it serves TLS with. A plan with Services is refused without the gateway capability. Changing a Network or a Volume is refused
(uninstall or rename instead). Each change gets an action (create, update, delete, unchanged) and,
for workloads, a strategy with the reason when it falls back. The CLI shows a summary; `--verbose`
shows every changed field.

## 7. Applying

`deployment.Execute`:

1. Pre-flight: runtime support, capabilities, host ports and routes (against other releases and
   other processes); pulls every image before touching anything running.
2. Materializes Config and Secret files under `$KARGA_HOME/releases/<release>/<revision>/`.
3. Deletes removed workloads, then creates and updates resources in dependency order. Before a
   workload with `dependsOn`, the workloads changed so far are waited for, the release proxy is
   started early, and karga waits until it routes to the Services the workload depends on.
4. Waits for recreated workloads; ones that do not become ready are stopped, not removed, so their
   logs can be read. A recreated workload's new container mounts the same named volumes, and the
   anonymous volumes Docker made for image `VOLUME` paths no mount covers are moved to it (as
   docker compose does); they are deleted only with the workload for good (removed from the
   package or uninstalled). A workload with such volumes is never replaced.
5. Updates the release proxy and the gateway, then deletes removed networks and volumes (a removed
   volume only with `retention: delete`, and only when the upgrade or rollback was given
   `--delete-volumes`: without it the command stops before changing anything;
   `uninstall --delete-volumes` removes them all).

Progress events (pull, create, wait, switch, drain, stop, done) are reported as they happen.

### Update strategies

- **recreate** (default): create the new container while the old one still runs (a spec Docker
  refuses leaves the old one serving), then stop the old one (SIGTERM, then a kill after the
  workload's `stopTimeout`, default 10s, which is also set on the container for `docker stop`),
  start the new one and remove the old one. Short outage: from the stop to the new one's start.
  The new container takes over the old one's anonymous volumes; only one of the two runs at a time.
- **replace:** start the new container next to the old one, wait until healthy, switch traffic,
  let the old one drain for `drainTimeout` and remove it; nothing is renamed. Needs a health check
  and read-only volumes (two writers on the same data are unsafe); otherwise the plan falls back to
  recreate and says why. An interrupted replace is repaired before the next plan: of the two
  containers the one the release proxy routes to is kept and recorded.

A workload's container is named `<release>-<workload>-<hash>`, the hash being the first 8 hex
digits of the workload's digest (its spec and the contents of the Configs and Secrets it mounts),
like a Kubernetes pod-template-hash: the same spec keeps its name, a changed one gets a new name.
The state records each container's name; karga finds the containers of a release by label
(`io.karga.release`, `io.karga.name`), never by building the name. A failed replace leaves its new
container stopped next to the old one, for its logs; the next deployment removes it. The release
proxy is named the same way (`<release>-karga-proxy-<hash>`, D37); Networks and Volumes
(`<release>-<name>`) keep fixed names.

### Volume backup and restore (D45)

Both copy files through a workload that mounts the volume (`ExportPath`/`ImportPath`, no helper
container), and stop for the time of it the volume's writers (a backup; a restore stops its readers
too) and what depends on them through `dependsOn`, read from the current revision, dependents first.
They start them again in dependency order, each once ready (the deployment's wait), and record the
downtime on the operation. A backup copies to a temporary file, starts the workloads, then sends
it, with its result in a trailer; workloads that do not get ready fail it with `not_ready`, the
file complete. A restore checks the backup, the revision files it needs and the room for a restore
point, keeps the upload under `$KARGA_HOME/restores`, stops the workloads, copies the volume to its
restore point (`$KARGA_HOME/restore-points/<release>/<volume>.tar.gz`), then empties the volume by
removing the containers mounting it (Docker keeps a mounted volume), removing and creating it again
and creating the same containers (name, labels, revision, anonymous volumes) without starting them,
writes the backup at the mount through one of them and starts them in order. `merge` skips emptying
it; `undo-restore` restores the restore point the same way.

## 8. Traffic: Services, the release proxy and the gateway (ADR-0011)

Karga's Envoys need the runtime capability `karga.host-gateway` (Linux with a local, rootful Docker daemon:
bind-mounted configuration and admin sockets, inotify, the host network). Without it a plan with
Services is refused.

- **Release proxy** (`<release>-karga-proxy-<hash>`, one per release with Services) joins the networks of
  the Services' workloads and carries the Service names as aliases on them. It listens on every
  Service port and forwards to the backend alias of the container currently running the Service's
  workload (`karga-<workload>-r<revision>`). Workloads carry no alias of their own name (Docker
  still resolves the container name, which changes with every update: reach a workload through its
  Service). One proxy has one address, so a port number belongs to one Service of the release. It
  publishes no port (D42). Every proxy also joins the edge network `karga-edge`: one Docker bridge
  for the host, `internal` (no way out), with traffic between its containers off
  (`enable_icc=false`), so releases stay apart while the host, and so the gateway in its network,
  reaches every address on it. Its range is karga-server's `--edge-subnet` (`KARGA_EDGE_SUBNET`,
  default `10.213.0.0/16`); it cannot change while proxies use it. Each release gets a pair of
  addresses, given in order from a counter in `$KARGA_HOME/edge.yaml` (an address a release leaves is
  not given again until the counter wraps); a proxy takes the one its predecessor does not hold, so
  a replace alternates between the two, and records it in its `io.karga.edge-ip` label. The gateway
  forwards to `<edge address>:<service port>`: the proxy already listens on every Service port, so
  exposing one changes nothing of the proxy. What the release exposes (listener, host, path,
  certificate directory) and the proxy container the gateway routes to are written to
  `$KARGA_HOME/proxies/<release>/expose.yaml`, so changing a host or path needs no new container.
  Docker cannot change a running container's ports or aliases: a changed proxy (an exposed port, a
  Service added or removed, a new image) is a new container, started next to the old one (D37).
  Both read the directory's `lds.yaml` and `cds.yaml`; each has its own bootstrap and admin socket
  in `proxies/<release>/<hash>/`. Once the new one routes, the gateway switches to it (the expose
  file names it; while two are labeled the gateway uses that one), the old one drains
  (`/drain_listeners?graceful`, up to 30s for its connections) and is removed. In the release's
  networks both answer to the Service names meanwhile; a client that cached the old address past
  that fails, as after any container change. An interrupted switch is repaired before the next
  plan: of the two proxies the one the gateway routes to is kept.
- **Gateway** (`karga-gateway-<hash>`, one per host, host network, found by its label
  `io.karga.component=gateway`) is configured by the host's
  administrator (D40): a document of kind `Gateway` the user keeps and applies (`karga gateway
  apply -f`), or the gateway commands. Each apply is a revision in `$KARGA_HOME/gateway-settings/`
  (`<n>.yaml` and `history.yaml`: when, who, from what; the last 10 kept; the highest is in force).
  An apply is checked first (strict YAML, values, listeners in use, ports, risks needing
  confirmation, then Envoy's own `--mode validate` on the configuration as static resources, in a
  throwaway container with copies of the certificates; it binds nothing); if the gateway then does
  not route (a listener Envoy rejects included), the
  revision is dropped and the gateway synced with the one before. The settings are the addresses
  it listens on (default `127.0.0.1`) and
  its listeners (name, port, protocol `tcp`, `http` or `https`). Packages only name listeners. The
  configuration is rebuilt under `$KARGA_HOME/gateway.lock` from the settings, the proxies' labels and
  the releases' expose files: one Envoy listener per used listener and address; HTTP listeners with
  virtual hosts by `Host` and path (D43: `pathType` exact as `path` first, then prefixes as
  `path_separated_prefix` longest first, then `/`; `to` is a `regex_rewrite` of the prefix, which also goes in
  `x-forwarded-prefix`); HTTPS listeners with one filter chain per host (SNI); a
  redirect to HTTPS on the http listeners with `redirectHTTPS` (each host to the port of the https
  listener serving it; the default `http` listener on 80 has it); clusters to the proxies' upstreams. Pre-flight
  (`gateway.Check`) refuses exposures on unknown listeners or listeners of another protocol, tcp
  listeners, hosts and paths or certificates another release holds, and listeners a host process holds.
  HTTP routes have no overall deadline (uploads, server-sent events and long polls run until done)
  and pass WebSocket upgrades through; a stream idle for 5 minutes is closed. Against slow clients
  (Envoy's edge guidance) request headers must arrive within 10s, an idle connection closes after
  an hour and a connection buffers at most 32 KiB beyond a body limit; https is TLS 1.2 or newer.
  `x-forwarded-for` is set to the client address alone, dropping what the client sent. A release
  whose expose file or certificate cannot be read is left out of the gateway with a warning; the
  others are still routed. An exposure the gateway does not route with is taken back. A settings
  change reads, writes, applies and reverts under the gateway lock, so changes at once all land.
  Only connect
  failures are retried. A listener's options (the administrator's, in the gateway settings) change that:
  a route timeout, the idle timeout (HTTP stream or `tcp_proxy`), a body limit (the `buffer`
  filter, at most 16Mi as it holds the body in memory), a rate limit (`local_ratelimit`, one token
  bucket per HTTP connection manager: per listener, per host on https) and, on tcp listeners, a
  PROXY protocol header (`upstream_proxy_protocol` on a cluster of its own; the release proxy passes
  the bytes through). They only rewrite the gateway's files.

The gateway container is named after what it is made from (`karga-gateway-<8 hex>` of its
`KARGA_HOME`, image and limits). When that changes (a karga version with a new Envoy, another
`KARGA_PROXY_IMAGE`) the gateway is handed over without an outage (D38): the new one starts with
its own bootstrap and admin socket in `gateway/<hash>/` and binds the same ports, which every
listener opens with `SO_REUSEPORT` (Envoy runs with `--disable-hot-restart`, whose socket two
Envoys in the host network would share). Once it routes every exposed port, the old one closes its
listeners (`/drain_listeners`: the kernel sends new connections to the new one), its connections
finish (up to 30s), and it is removed. If the new one does not become ready it is removed and the
old one keeps serving. A crash is not covered: the restarted container serves again.

A replace switches only the release proxy: the gateway forwards to the proxy, which stays. A workload
no Service targets receives no traffic and is replaced without a switch. Configuration is built from
Envoy's protobuf types, validated, and written atomically as files that Envoy watches; listeners are
written only once Envoy knows their clusters. karga waits for Envoy's admin API (a unix socket) to
report the new backend healthy and used by an active listener before it continues, and reverts on
failure.

## 9. Health checks

- `exec`: Docker's healthcheck runs a command in the container.
- `tcp` and `http`: karga checks the port from the host at the container's address, so the image
  needs no tool.

All take `interval`, `timeout`, `retries` and `startPeriod`; failures during the start period do not
count.

The workload's health check is the only one: the release proxy and the gateway run no health checks
of their own (D44). karga routes to a new container only once its check passed, and repairs one that
fails later. A `tcp` check is an empty connection; services that penalize those (sshd since OpenSSH
9.8, fail2ban) need an `http` or `exec` check.

## 10. Security model

- One trust level (ADR-0016): whoever can open the karga socket (group `karga`) administers the
  host, as with the Docker socket. There are no users, roles or remote access.
- Containers are never privileged and run with `no-new-privileges` by default; the `security` block
  adds a read-only root filesystem, capability changes and a user. A package may add only Docker's
  default capabilities unless the administrator allows more (`--allow-capabilities`). The gateway
  runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE`; release proxies drop every
  capability and run with `no-new-privileges`.
- Packages cannot open ports: workloads publish nothing, release proxies publish only loopback
  upstreams, and the gateway serves only the listeners the administrator defines, on `127.0.0.1`
  unless told otherwise.
- Secret files are mounted read-only from a 0700 directory; secret values never appear in output,
  logs (the redactor learns every value it materializes, and at start the current revisions') or
  the state (only keyed digests); `writeOnly` values are stored as `<redacted>`, and a
  package that uses one outside a Secret is refused.
- Config and Secret directories are updated in place: running containers keep seeing them.
- A package cannot exhaust the server: rendering is bounded in depth, time and output, and archives
  in size and number of files.
- Memory limits also cap swap; every container has a process limit and log rotation.
- Templates cannot read the host environment or the network.

## 11. Testing

- Unit tests with the fake runtime (`make test`, with `-race`). A golden file
  (`internal/application/releases/testdata/lifecycle.golden.yaml`) pins what each lifecycle step
  does to the state and the runtime; `internal/server` tests the API through the composition root.
- The runtime contract suite against Docker, including what Docker actually enforces (limits,
  logging, security options).
- Integration tests against Docker on Linux (`make test-integration`, or `make test-linux` in a VM),
  run in parallel with one shared `KARGA_HOME`.
  Zero-loss claims are measured with clients on the host and on the release network.
- End-to-end tests drive the built binaries, karga-server and the karga CLI built from its
  repository (`make test-e2e`), including a server killed mid-upgrade that resumes it.
- The CLI's own tests run against a fake `Backend` behind the SDK's HTTP handler (`apitest.Serve`),
  with golden files for the text output; the SDK's tests cover the package engine, plans and the
  API's version check and detached operations, and fuzz archive unpacking, resource decoding and
  secret paths.
