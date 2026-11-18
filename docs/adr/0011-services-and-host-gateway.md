# ADR-0011: Services and an administrator-owned gateway

**Status:** accepted; supersedes the host port and route parts of ADR-0010

**Decision:** Exposure moves out of workloads, in the spirit of Kubernetes Services and the Gateway API:

- **Workloads expose nothing.** They declare named container ports and publish none; their package
  names are no DNS names. Each carries only a per-revision backend alias meant for the release proxy
  (Docker still resolves container names on user networks; it cannot be turned off).
- **Service** (`kind: Service`) gives one workload, chosen by name, a stable name inside its release.
  It maps service ports to the workload's ports. The release proxy always serves it: it carries the
  Service names as aliases on the workload's networks and forwards to the backend alias of the
  current container. Releases are isolated: a Service name resolves only on its release's networks.
- **Gateway** settings belong to the host's administrator (`karga gateway listener add|remove`,
  `karga gateway set listen`), stored in `$KARGA_HOME/gateway.yaml`: listen addresses (default
  `127.0.0.1`) and named listeners (port and protocol `tcp`, `http` or `https`). A Service exposes a
  port on a listener by name. A package cannot open a listener, choose an address or take a port
  another release holds; an unknown listener is refused before anything changes.
- The gateway forwards to the release proxies' loopback upstreams, not to workloads, so a replace
  switches only the release proxy. What a release exposes lives in a file next to its proxy
  configuration; changing a host or path does not recreate the proxy.
- `hostPort`, `hostIP` and `routes` are removed from the Workload, in `v1alpha1` itself. Without the
  gateway capability (macOS, a remote Docker host) a plan with Services is refused: karga targets
  Linux.

**Reason:** A package should not decide what a host exposes; one place, owned by the administrator,
should. Every client going through the release proxy makes every Service switch without loss and
keeps the gateway's configuration stable during replaces. One proxy per release keeps the resource
cost low (about 15 MB per Envoy).

**Constraints:** One release proxy has one address, so a service port number belongs to one Service
of the release. Services carry TCP only. Exposing a new port recreates the release proxy (a brief
interruption of its connections). Workloads of a release on the same network can still reach each
other by IP; there is no network policy.
