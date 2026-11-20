// Package proxyops holds what the deployment executor, the release proxy and the host gateway
// share: runtime object names and removal, host port checks, and control of karga's Envoys.
package proxyops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Remover is what removing karga's workloads needs of a container runtime.
type Remover interface {
	GetWorkload(ctx context.Context, name string) (ports.WorkloadStatus, error)
	StopWorkload(ctx context.Context, name string, timeout time.Duration) error
	DeleteWorkload(ctx context.Context, name string) error
	DeleteVolume(ctx context.Context, name string) error
}

// RuntimeName is the runtime object name of a release-local resource name (a Network, a Volume).
func RuntimeName(release, name string) string { return release + "-" + name }

// workloadHash is how many hex digits of its digest end a workload container's name.
const workloadHash = 8

// WorkloadName is the container name of a workload with digest ("<algorithm>:<hex>"): it ends with
// the digest's first hex digits, like a Kubernetes pod-template-hash, so the same spec keeps its
// name and a changed one gets a new name next to the old.
func WorkloadName(release, workload, digest string) string {
	_, hex, _ := strings.Cut(digest, ":")
	return RuntimeName(release, workload) + "-" + hex[:min(workloadHash, len(hex))]
}

// RemoveWorkload stops a container gracefully and deletes it with its anonymous volumes; a missing
// one is fine.
func RemoveWorkload(ctx context.Context, rt Remover, name string) error {
	if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return err
	}
	return DeleteWorkload(ctx, rt, name)
}

// DeleteWorkload deletes a container for good, with the anonymous volumes the runtime made for its
// image's VOLUME paths (nothing else can use them); a missing one is fine.
func DeleteWorkload(ctx context.Context, rt Remover, name string) error {
	st, err := rt.GetWorkload(ctx, name)
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := rt.DeleteWorkload(ctx, name); err != nil {
		return err
	}
	var errs []error
	for _, m := range st.AnonymousVolumes {
		errs = append(errs, rt.DeleteVolume(ctx, m.Source))
	}
	return errors.Join(errs...)
}

// IPOverlap reports whether two bind addresses can conflict: equal, or either binds every address.
func IPOverlap(a, b string) bool {
	if a == b {
		return true
	}
	unspecified := func(s string) bool {
		ip := net.ParseIP(s)
		return s == "" || (ip != nil && ip.IsUnspecified())
	}
	return unspecified(a) || unspecified(b)
}

// Host is what karga's proxies and probes need from the host: the proxies' control, the host's
// network, and whether the runtime can run the proxies. karga-server makes one and passes it down.
type Host struct {
	Proxies ports.ProxyControl
	Net     ports.HostNet
	// EdgeSubnet is the address range of the network the gateway reaches the release proxies on
	// (D42; empty: DefaultEdgeSubnet).
	EdgeSubnet string
	// supported says whether the runtime can run karga's proxies: they rely on bind-mounted unix
	// sockets, inotify and (the gateway) the host network namespace (a local, rootful Docker on Linux).
	supported atomic.Bool
}

// DefaultEdgeSubnet is the edge network's range unless karga-server is told another one.
const DefaultEdgeSubnet = "10.213.0.0/16"

// Edge returns the edge network's range.
func (h *Host) Edge() string {
	if h.EdgeSubnet == "" {
		return DefaultEdgeSubnet
	}
	return h.EdgeSubnet
}

// ValidateEdgeSubnet checks an edge network range: IPv4, from /16 to /28.
func ValidateEdgeSubnet(s string) error {
	p, err := netip.ParsePrefix(s)
	switch {
	case err != nil:
		return fmt.Errorf("edge subnet %q: %w", s, err)
	case !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 28:
		return fmt.Errorf("edge subnet %q: want an IPv4 range from /16 to /28", s)
	case p.Masked() != p:
		return fmt.Errorf("edge subnet %q: want the range's first address, %s", s, p.Masked())
	}
	return nil
}

// UseRuntime records whether the runtime described by info can run karga's proxies.
func (h *Host) UseRuntime(info ports.RuntimeInfo) { h.supported.Store(info.Has(ports.CapHostGateway)) }

// Supported reports whether the runtime can run karga's proxies.
func (h *Host) Supported() bool { return h.supported.Load() }

// ProxyReadyTimeout bounds how long a proxy may take to route to a new backend.
const ProxyReadyTimeout = 30 * time.Second

// ProxyDrainTimeout bounds how long a replaced proxy or gateway waits for its connections to end.
const ProxyDrainTimeout = 30 * time.Second

// EnvoyErr adds what an Envoy container logged about rejected configuration to err, so a config
// Envoy refused (e.g. a port it cannot bind) is reported instead of only a timeout.
func EnvoyErr(ctx context.Context, rt interface {
	Logs(ctx context.Context, name string, tail int, follow bool, w io.Writer) error
}, container string, err error) error {
	if err == nil {
		return nil
	}
	var out strings.Builder
	if lerr := rt.Logs(ctx, container, 50, false, &out); lerr != nil {
		return err
	}
	var why []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "rejected") || strings.Contains(line, "][error]") || strings.Contains(line, "][critical]") {
			why = append(why, strings.TrimSpace(line))
		}
	}
	if len(why) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s logged:\n  %s", err, container, strings.Join(why[max(0, len(why)-3):], "\n  "))
}
