# Roadmap and known limitations

What karga does not do yet, what it does with known limits, and the open questions. The design is in
[`design.md`](design.md); decisions are in [`adr/`](adr/).

## Planned or considered

| Feature | Notes |
|---------|-------|
| Lifecycle tasks | Pre/post install and upgrade tasks (e.g. database migrations); needs an execution model. |
| Automatic certificates | ACME for TLS routes; today certificates come from a Secret. |
| `karga doctor` | One command checking the Docker version, gateway support, `KARGA_HOME` permissions, ports 80/443 and the Envoy image. |
| Remote management | The API is on a local unix socket only; managing another host means logging in to it. |
| Cloud service | A hosted service for managing several hosts, in a separate private repository; it would use karga-sdk's API. |
| Private registries | Image pulls are anonymous today; credentials from the Docker configuration. |
| Encrypted secret store | Values that are not `writeOnly` are stored as written. |
| UDP and client IP for TCP | Services carry TCP only; TCP backends do not see the client address (HTTP and HTTPS listeners pass `x-forwarded-for`). |
| Network policy | Workloads of a release on the same network can reach each other by IP; only Service names are resolved. |
| Services across releases | A Service is reachable only inside its release; other releases go through the gateway. |
| Podman adapter | REST API or Quadlet; the runtime contract suite defines what it must do. It also needs the gateway and release proxy to work with it. |
| Package distribution | OCI artifacts or a repository index, signing and provenance. |
| Package dependencies | One package is one release today (inside a release, `dependsOn` orders workloads). |
| Runtime-specific extensions | A namespaced block for settings outside the portable model. |
| Parallel apply | Independent resources are applied one after the other today. |

## Known limitations

- **One karga-server per host.** The gateway is shared by the host; two servers with different
  `KARGA_HOME`s would fight over it.
- **Repairs** recreate a removed volume empty; settings changed inside a running container (not in
  its spec) are not detected.
- **Linux only.** Services need the gateway and the release proxy, which need Linux with a local
  Docker daemon; elsewhere a plan with Services is refused.
- **Gateway.** One container serves every release's outside traffic; it restarts automatically but
  is a single point. Local users can reach the loopback upstream ports directly. Image or limit
  changes recreate it (a short interruption).
- **Release proxy.** One per release with Services (about 15 MB). A service port number belongs to
  one Service of the release. Adding or removing a Service reconnects it to its networks, and
  exposing a new port recreates it, briefly interrupting its connections.
- **Replace** needs a health check and read-only volumes; stateful workloads are recreated.
- **Volume backups** go through a workload that mounts the volume; a live backup is not consistent
  for databases (use `--stop` or the database's own dump). Restore overwrites files with the same
  names and keeps the others.
- **Anonymous volumes** (image `VOLUME` paths no Volume covers) move to the new container on
  upgrades, but `uninstall` deletes them and `volume backup` cannot reach them; karga warns.
- **State backup** does not include `state.key`; without it, Secrets are applied once more after a
  restore.
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
| An interrupted operation leaves a release locked or half applied. | Operations run in karga-server, independent of the client; a restarted server resumes the ones it left; `--force` aborts one left by another process; interrupted replaces are repaired before the next plan. |
| A Podman adapter may not fit the Docker-shaped capability model. | The contract suite and capabilities are runtime-agnostic. |

## Open questions

- How lifecycle tasks run: as one-off workloads, inside an existing container, or on the host.
- Podman through its API or through Quadlet units.
- How packages are distributed and signed.
