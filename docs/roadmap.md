# Roadmap and known limitations

What karga does not do yet, what it does with known limits, and the open questions. The design is in
[`design.md`](design.md); decisions are in [`adr/`](adr/).

## Planned or considered

| Feature | Notes |
|---------|-------|
| Lifecycle tasks | A `Task` kind: one-off containers that run before or after an install or upgrade (database migrations), must exit 0, and stop the operation when they fail; their output is kept with the operation. Today migrations run in the application's entrypoint. |
| Replicas | `replicas: N` for stateless workloads (no read-write volume); the release proxy balances across them; karga marks a replica whose health check fails unhealthy in the proxy's cluster file (no active Envoy checks, D44). Today a workload is one container. |
| Automatic certificates | ACME for TLS routes; today certificates come from a Secret. |
| Certificate guide | How to get and renew certificates for the gateway (e.g. certbot with a DNS challenge: the gateway holds ports 80 and 443, so an HTTP challenge on them conflicts). Today the operator gets them and renews by upgrading with the new Secret. |
| Certificate expiry warning | `karga status` and `gateway status` show when each certificate expires and warn 14 days before; today renewal is an upgrade with the new Secret, and nothing reminds you. |
| Repair notifications | Tell someone (a webhook) when the reconciler gives up on a resource; today `karga status` exits 6 and can be polled by a monitor. |
| Scheduled tasks | Periodic jobs and volume backups run by karga; today a systemd timer on the host runs `karga volume backup`. |
| Route matching | Header, method or regex matching and weighted backends; exposures match a path (`pathType` prefix or exact) and rewrite a prefix (`to`) today (D43). |
| Gateway options | HSTS, compression, bodies over 16Mi without buffering; listeners already take a timeout, idle timeout, body limit, rate limit and PROXY protocol (D35). |
| Log shipping | A Docker log driver per workload (journald, syslog, fluentd); today logs rotate in json-file and are read with `karga logs`. |
| `karga doctor` | One command checking the Docker version, gateway support, `KARGA_HOME` permissions, ports 80/443 and the Envoy image. |
| Remote management | The API is on a local unix socket only; managing another host means logging in to it. |
| Private registries | Image pulls are anonymous today. Credentials would come from the server user's Docker login (`sudo -u karga docker login <host>`) and be passed with each pull. |
| Encrypted secret store | Values that are not `writeOnly` are stored as written. |
| UDP | Services carry TCP only. TCP backends see the client address only with a listener's `--proxy-protocol` (HTTP and HTTPS listeners pass `x-forwarded-for`). |
| Network policy | Workloads of a release on the same network can reach each other by IP; only Service names are resolved. |
| Services across releases | A Service is reachable only inside its release; other releases go through the gateway. |
| Podman adapter | REST API or Quadlet; the runtime contract suite defines what it must do. It also needs the gateway and release proxy to work with it. |
| Package distribution | OCI artifacts or a repository index, signing and provenance. |
| Package dependencies | One package is one release today (inside a release, `dependsOn` orders workloads). |
| Runtime-specific extensions | A namespaced block for settings outside the portable model. |
| Parallel apply | Independent resources are applied one after the other today. |

## Not planned

| Feature | Why |
|---------|-----|
| Automatic rollback (`--atomic`) | A failed upgrade stays failed (D2): a recreated workload that does not become ready is stopped, its logs can be read, and the operator fixes it with another upgrade or `karga rollback`; `replace` avoids the outage where it applies. |
| Default resource limits | A workload without `resources` has no memory or CPU limit; the package sets its own. |

## Known limitations

- **One karga-server per host.** The gateway is shared by the host: a server whose `KARGA_HOME`
  is not the gateway's refuses to expose anything (and never removes the other's gateway); remove
  it with Docker when moving to another home
  (`docker rm -f $(docker ps -aq --filter label=io.karga.component=gateway)`).
- **Repairs** recreate a removed volume empty; settings changed inside a running container (not in
  its spec) are not detected.
- **Linux only.** Services need the gateway and the release proxy, which need Linux with a local
  Docker daemon; elsewhere a plan with Services is refused.
- **Gateway.** One container serves every release's outside traffic; it restarts automatically but
  is a single point: a crash interrupts traffic until it is back. Local users can reach the release
  proxies' edge addresses directly (they bypass the gateway's limits). A new image or limits hand over to a new gateway without an outage
  (D38); a connection still open on the old one after 30s is closed.
- **Release proxy.** One per release with Services (about 15 MB; two for a moment while one
  replaces the other). A service port number belongs to one Service of the release. A client in
  the release's networks that caches the proxy's address beyond the 30s drain of a replaced proxy
  loses its connection.
- **Replace** needs a health check and read-only volumes; stateful workloads are recreated.
- **Volume backups** go through a workload that mounts the volume; a live backup is not consistent
  for databases (use `--stop` or the database's own dump). Restore overwrites files with the same
  names and keeps the others.
- **Anonymous volumes** (image `VOLUME` paths no Volume covers) move to the new container on
  upgrades, but `uninstall` deletes them and `volume backup` cannot reach them; karga warns.
- **Backups** of the state are copies of `$KARGA_HOME` with the server stopped; copy `state.key`
  with it, or every Secret is applied once more after a restore.
- **Secrets** that are not marked `writeOnly` in the schema are stored as written in the state.
- **Images** are pulled anonymously.
- **Package format** `karga.io/v1alpha1` may still change.
- **Releases** are installed from local directories or archives only.

## Risks

| Risk | Mitigation |
|------|------------|
| Access to the Docker socket is root-equivalent. | Documented; containers are never privileged; `no-new-privileges` by default. |
| The gateway runs as root (to bind low ports in the host namespace). | Every capability but `NET_BIND_SERVICE` and `DAC_OVERRIDE` is dropped, `no-new-privileges`, digest-pinned image. |
| A failed recreate leaves the workload down. | Pre-flight checks and image pulls happen before anything stops; the plan says recreate; replace where possible; `rollback`. |
| An interrupted operation leaves a release locked or half applied. | Operations run in karga-server, independent of the client; a restarted server resumes the ones it left; `--force` aborts one an earlier process left; interrupted replaces are repaired before the next plan. |
| A Podman adapter may not fit the Docker-shaped capability model. | The contract suite and capabilities are runtime-agnostic. |

## Open questions

- How lifecycle tasks run: as one-off workloads, inside an existing container, or on the host.
- Podman through its API or through Quadlet units.
- How packages are distributed and signed.
