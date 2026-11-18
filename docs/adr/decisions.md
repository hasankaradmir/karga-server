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
| D15 | Interrupted operations | PID, hostname and the request's options are recorded; karga-server resumes them at start with those options, or a dead operation is aborted with `--force` (ADR-0015). |
| D16 | Replace strategy | `updateStrategy: {type: replace, drainTimeout}`: the new container starts next to the old one; eligible with read-only volumes and a health check. |
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

D23 was not used.
