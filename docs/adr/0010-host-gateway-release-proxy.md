# ADR-0010: Host gateway and release proxy (Linux)

**Status:** accepted; host ports, routes and the replace-only release proxy superseded by ADR-0011

**Decision:** Traffic goes through two kinds of karga-managed Envoy:

- **Gateway:** one container per host (`karga-gateway`, host network) serves every tcp `hostPort`
  and HTTP(S) route of every release. Workloads only publish a loopback upstream port chosen by
  karga; the gateway's clusters point at those. Its configuration is rebuilt from the running
  workload containers' labels under a host-wide lock; switches of replaces in progress are stored
  so concurrent updates by other releases keep them. TLS routes get a certificate per host (SNI)
  and a redirect from port 80.
- **Release proxy:** one container per release carries the network aliases of its `replace`
  workloads, so network clients switch without loss too. Recreate workloads (e.g. databases) are
  reached directly.

Both run the digest-pinned distroless Envoy 1.36.13 with one worker thread and limits (128Mi, 0.5
CPU, 256 processes). The gateway runs as root with only `NET_BIND_SERVICE` and `DAC_OVERRIDE`
(binding low ports in the host namespace needs it; Docker grants no ambient capabilities). The
configuration is built from Envoy's own protobuf types and validated before it is written.

**Reason:** Low resource use is karga's main principle: one Envoy per host plus at most one per
release instead of one per workload. A spike measured host network to loopback upstreams working,
listeners added without restarts, 0 failed requests over 20 switches (4574 requests), a 31 ms
gateway restart and about 15 MB of memory per Envoy.

**Constraints:** The gateway is a single point for host traffic (restarted automatically). Local
users can reach the proxies directly: the loopback upstream ports, the edge addresses since D42. A remote Docker host is not supported (bind
mounts, unix sockets and the host network must be local). Replace workloads of one release cannot
share a container port behind the release proxy.
