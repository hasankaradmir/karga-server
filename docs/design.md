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
operations. When it starts, it resumes the operations it left running before it answers requests
or repairs anything (section 5).

**Reconciliation.** Desired state is each release's current revision and its applied resources;
only install, upgrade and rollback change it. karga-server's reconciler (`internal/application/
reconciler`, the loop; `releases.Service.Reconcile`, the repairs) brings the runtime back to it:

- *Triggers:* Docker events of karga's containers (die, stop, destroy, oom, unhealthy; 2 s debounce),
  a pass over every release every 30 s (`--reconcile-interval`), the server's start, and every
  install, upgrade or rollback. One release at a time.
- *Scope:* deployed releases that are not paused and have no operation running. A failed release is
  the user's (D2). Within the server a per-release mutex serializes commands and repairs: a command
  waits for a repair; a repair skips a busy release.
- *Repairs:* missing workloads, proxies, networks and volumes and containers whose `io.karga.digest`
  label differs from the applied digest (changed outside karga) are created from the current revision
  through the usual plan and executor, creates only, without a new revision; stopped containers are
  started; unhealthy ones (Docker's check, or karga's tcp/http check failing `retries` times) are
  restarted; the gateway is synced if something is exposed.
- *Backoff:* per resource, 10 s doubling to 5 min; after 5 failures in 10 minutes it gives up until
  the user acts on the release or runs `karga reconcile`.
- Every repair is an operation of type `reconcile`; `karga events` lists them.

```
cmd/karga-server                entry point
internal/server                 karga-server: state, runtime, socket, peer credentials; backend.go maps
                                the API (karga-sdk/api) to the release service and back
internal/application/releases   the release service: one Core (state, runtime, locks, deploy steps)
                                shared by Lifecycle (install, upgrade, rollback, uninstall, resume,
                                repair), Reads (status, history, logs, exec), Volumes and Gateway
internal/application/deployment plan (diff, strategies), execute, release proxy, probes (deployment.Env)
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
most unreferenced files. `version.yaml` holds the format version; a newer one is refused.

Every mutating command records an operation first. An operation whose process is alive on this
host locks the release; one whose process is gone is aborted only with `--force`. Operations do
not depend on their client's connection. When karga-server starts it resumes the operations it
left running, with the options recorded on the operation (`--delete-volumes`, `--strategy`,
`--timeout`, `--keep-history`): install, upgrade and rollback deploy their recorded revision again
(the plan skips what is done; no new revision), uninstall runs again, others are aborted
(ADR-0015). The finishing operation is recorded as `resume`. A failed apply
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

- **recreate** (default): stop the old container (SIGTERM, then a kill after the workload's
  `stopTimeout`, default 10s, which is also set on the container for `docker stop`), start the new
  one. Short outage.
- **replace:** start `<name>-next` next to the old container, wait until healthy, switch traffic,
  let the old one drain for `drainTimeout`, remove it and rename `<name>-next`. Needs a health
  check and read-only volumes (two writers on the same data are unsafe); otherwise the plan falls
  back to recreate and says why. An interrupted replace is repaired before the next plan.

## 8. Traffic: Services, the release proxy and the gateway (ADR-0011)

Karga's Envoys need the runtime capability `karga.host-gateway` (Linux with a local, rootful Docker daemon:
bind-mounted configuration and admin sockets, inotify, the host network). Without it a plan with
Services is refused.

- **Release proxy** (`<release>-karga-proxy`, one per release with Services) joins the networks of
  the Services' workloads and carries the Service names as aliases on them. It listens on every
  Service port and forwards to the backend alias of the container currently running the Service's
  workload (`karga-<workload>-r<revision>`). Workloads carry no alias of their own name (Docker
  still resolves the container name, `<release>-<workload>`). One proxy has one address, so a port number belongs to one Service of the release. The
  exposed Service ports are published on loopback upstream ports chosen by karga and recorded in a
  container label (`io.karga.gateway`); adding one recreates the proxy. What the release exposes
  (listener, host, path, certificate directory) is written to `$KARGA_HOME/proxies/<release>/expose.yaml`,
  so changing a host or path needs no recreate.
- **Gateway** (`karga-gateway`, one per host, host network) is configured by the host's
  administrator in `$KARGA_HOME/gateway.yaml`: the addresses it listens on (default `127.0.0.1`) and
  its listeners (name, port, protocol `tcp`, `http` or `https`). Packages only name listeners. The
  configuration is rebuilt under `$KARGA_HOME/gateway.lock` from the settings, the proxies' labels and
  the releases' expose files: one Envoy listener per used listener and address; HTTP listeners with
  virtual hosts by `Host` and path prefix; HTTPS listeners with one filter chain per host (SNI); a
  redirect to HTTPS on the port-80 http listener; clusters to the proxies' upstreams. Pre-flight
  (`gateway.Check`) refuses exposures on unknown listeners or listeners of another protocol, tcp
  listeners, hosts and paths or certificates another release holds, and listeners a host process holds.
  HTTP routes have no overall deadline (uploads, server-sent events and long polls run until done)
  and pass WebSocket upgrades through; a stream idle for 5 minutes is closed. Only connect
  failures are retried.

A replace switches only the release proxy: the gateway forwards to the proxy, which stays. A workload
no Service targets receives no traffic and is replaced without a switch. Configuration is built from
Envoy's protobuf types, validated, and written atomically as files that Envoy watches; listeners are
written only once Envoy knows their clusters. karga waits for Envoy's admin API (a unix socket) to
report the new backend healthy and used by an active listener before it continues, and reverts on
failure.

## 9. Health checks

- `exec`: Docker's healthcheck runs a command in the container.
- `tcp` and `http`: karga checks the port from the host at the container's address, so the image
  needs no tool. Behind the release proxy Envoy checks the same HTTP path before routing to a new
  container.

All take `interval`, `timeout`, `retries` and `startPeriod`; failures during the start period do not
count.

## 10. Security model

- One trust level (ADR-0016): whoever can open the karga socket (group `karga`) administers the
  host, as with the Docker socket. There are no users, roles or remote access.
- Containers are never privileged and run with `no-new-privileges` by default; the `security` block
  adds a read-only root filesystem, capability changes and a user. A package may add only Docker's
  default capabilities unless the administrator allows more (`--allow-capabilities`). The gateway
  runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE`.
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
