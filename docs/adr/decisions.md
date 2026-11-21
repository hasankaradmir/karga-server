# Implementation decisions

Decisions taken while building karga, in order. Architecture decisions are the ADRs in this
directory.

| # | Topic | Decision |
|---|-------|----------|
| D1 | Scope | Single-host package manager first; registry, dependencies and other runtimes later. |
| D2 | Upgrades | Default strategy recreate, no automatic revert: a failed revision stays failed and `karga rollback` goes back. |
| D3 | Failed upgrades | The previous revision's state, files and history stay intact; a running container is not guaranteed. Narrowed by D18. |
| D4 | Dependencies | Deferred; one package is one release. |
| D5 | Stack | cobra, moby client, Sprig, santhosh-tekuri/jsonschema, Masterminds/semver, go.yaml.in/yaml, log/slog. |
| D6 | Secrets | Secret values become files mounted read-only from a 0700 directory; stored manifests keep only keyed digests (D22). |
| D7 | Naming | Template context `.Values`, `.Release`, `.Package`, `.Files`; values flags `-f`, `--set`, `--set-string`. |
| D8 | Determinism | Non-deterministic template functions are removed; `keys`/`values` are sorted. |
| D9 | Host ports | `hostIP` defaulted to 127.0.0.1; replaced by D26 (the gateway listens on 127.0.0.1 unless the administrator says otherwise). |
| D10 | Pull policy | `pullPolicy: always \| if-not-present \| never`, default if-not-present. |
| D11 | Config and Secret delivery | Bind mounts from `$KARGA_HOME/releases/<rel>/<rev>/` (0700 directory, 0644 files), updated in place (ADR-0015, ADR-0016). |
| D12 | Network and Volume changes | Refused in an upgrade (uninstall or rename instead). |
| D13 | History | `--history-max`, default 10 revisions. |
| D14 | Failed containers | Stopped, not removed, so their logs can be read. |
| D15 | Interrupted operations | PID, hostname and the request's options are recorded; karga-server resumes them at start with those options, or an interrupted operation is aborted with `--force` (ADR-0015). One left running is always an earlier process's: one karga-server runs per home and only it runs operations, so a PID another process took since (a reboot) does not keep the release locked. |
| D16 | Replace strategy | `updateStrategy: {type: replace, drainTimeout}`: the new container starts next to the old one; eligible with read-only volumes and a health check. Names: D34. |
| D17 | Strategy override | `--strategy recreate\|replace` on upgrade and rollback. |
| D18 | Harmless failures | A failure that touched nothing running (failed pull, unhealthy replace) keeps the release status. |
| D19 | Replace with a proxy | Per-workload Envoy (ADR-0009), later replaced by D20. |
| D20 | Traffic through Envoy | Host gateway and release proxy (ADR-0010); low resource use as the guiding principle. |
| D21 | Production, round 1 | Digest-pinned Envoy, validated configuration, Envoy rejection reasons in errors, state backup (later dropped, ADR-0017) and migration safety. |
| D22 | Production, round 2 | TLS routes with redirects, `karga logs`, gateway health and access log, keyed (HMAC) digests of secrets. |
| D24 | Production, round 3 | Swap and process limits, OOM reporting, log rotation, `tcp`/`http` health checks, `no-new-privileges` and the security block, volume backup and restore. |
| D25 | Developer and user experience | Parallel integration tests, `make test-linux`, live progress, `exec`, `diff`, `create` and notes, `status --watch`, colors, completion, gateway package, release automation. |
| D26 | Services and the host gateway | Workloads expose nothing; Services served by the release proxy; gateway listeners and listen addresses owned by the administrator; Linux only (ADR-0011). |
| D27 | Server and CLI | karga-server owns state and runtime; the CLI is its client on a local unix socket (ADR-0012). |
| D28 | Reconciliation | karga-server repairs deployed, unpaused releases toward their current revision (missing or changed objects, stopped or unhealthy containers), never creating revisions; backoff, then give up. |
| D29 | Start order | `dependsOn` names Services; the executor waits for them (ready and routed by the release proxy) before the dependent workload; not part of the workload's digest. |
| D30 | Template limits | Templates run under a budget: `range` loops at most 1,000,000 iterations per render, the time limit checked as they run, karga's own overflow-safe `until`, `seq`, `repeat` and `indent`; a render takes at most 10 s and writes at most 16 MiB. |
| D31 | Lifecycle rules | What an install, upgrade, rollback or prune may do is decided in `internal/domain/release` (`Install`, `CanUpgrade`, `CanRollback`, `Prunable`, the status transitions), without I/O, before an operation is recorded. |
| D32 | Confirmations | The SDK names each request's risk (`Risk()`); karga-server refuses one with a risk that is not `Confirmed`; the CLI asks with the same text, or takes `--yes` (ADR-0016). |
| D33 | Not planned | No automatic rollback (`--atomic`, D2 stands); no default resource limits (packages set theirs); the SDK's `packages` keeps its name (`package` is a keyword, and it names the domain's term); `ports.HostNet` has one implementation and no fake (Linux only, covered by the integration tests). |
| D34 | Container names | A workload's container is `<release>-<workload>-<8 hex of its digest>`: the same spec keeps its name, a changed one gets a new one, so a replace starts the new container under its own name and renames nothing. Containers are found by label and the state records their names; the proxy, Networks and Volumes keep fixed names. |
| D35 | Listener options | Timeout, idle timeout, body limit (at most 16Mi: Envoy buffers it), rate limit and PROXY protocol are options of a gateway listener, set by the administrator (`listener add`/`set`), never by packages; they only rewrite the gateway's files. |
| D36 | Recreate order and backend aliases | A recreate creates the new container before stopping the old one; every workload answers to its revision's backend alias from its creation, so a Service that starts or stops targeting it changes only the release proxy. |
| D37 | Release proxy replace | The release proxy is named `<release>-karga-proxy-<8 hex of its spec digest>`; a changed proxy starts next to the old one (own bootstrap and admin socket, shared listeners and clusters), the gateway switches to it through the expose file, and the old one drains and goes. Replaces the reconnect and recreate of D20 except on a name collision. |
| D38 | Gateway hand-over | The gateway is `karga-gateway-<8 hex of its source>`, found by label; a new source starts a second gateway on the same ports (`SO_REUSEPORT`, no hot restart), which takes over once it routes; the old one closes its listeners, drains up to 30s and goes. Measured: no failed connection in 9 hand-overs under load. |
| D39 | Gateway settings file | `gateway.yaml` edited by hand, checked and applied with `karga gateway validate|apply`; replaced by D40. |
| D40 | Gateway settings as documents | The settings are a document (`apiVersion: karga.io/v1alpha1`, `kind: Gateway`, `spec`) the user keeps; karga never writes it. `diff -f` and `apply -f` (the whole desired state) check it; each apply and each gateway command is a revision (last 10, with time, actor and source); `rollback` applies an earlier one; a revision the gateway does not route with is dropped. |
| D41 | Listener names and HTTPS redirects | The default listeners are `http` (80, `redirectHTTPS`) and `https` (443). An http listener redirects plain requests for the hosts an https listener serves only with `redirectHTTPS`, each to the port of the https listener serving the host; the port-80 rule is gone. |
| D42 | Gateway to proxies on the edge network | Release proxies publish no port: they join `karga-edge` (one internal bridge, `enable_icc=false`, range `--edge-subnet`, default 10.213.0.0/16) at their release's pair of addresses (given in order, alternating on a replace); the gateway in the host network forwards to `<address>:<service port>`. No NAT, docker-proxy or port race; exposing a port changes nothing of the proxy. Local users can still reach the addresses. A unix socket design was tried and dropped (mounts, debugging, an Envoy crash). |
| D43 | Exposure paths | `pathType` is `prefix` (the default: the path and below it, by segment, so `/api` is not `/apix`) or `exact`; `to` replaces a prefix in what the Service receives and sends it in `x-forwarded-prefix`. No header, method or regex matching, no weighted backends (roadmap). |
| D44 | One health source | Envoy (release proxy and gateway) runs no active health checks; the workload's own check, run by karga, is the only one. karga routes to a container once it passed and repairs it after. Envoy's TCP checks (an empty connection every 500ms) made sshd (`PerSourcePenalties`) and fail2ban lock out every client behind the proxy's address, and with one backend per cluster they changed no routing (panic mode). With replicas, karga will mark failing endpoints unhealthy in the cluster file. |

D23 was not used.
