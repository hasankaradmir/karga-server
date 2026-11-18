// Package proxyops holds what the deployment executor, the release proxy and the host gateway
// share: runtime object names and removal, host port checks, and control of karga's Envoys.
package proxyops

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// RuntimeName is the runtime object name of a release-local resource name.
func RuntimeName(release, name string) string { return release + "-" + name }

// RemoveWorkload stops a container gracefully and deletes it with its anonymous volumes; a missing
// one is fine.
func RemoveWorkload(ctx context.Context, rt ports.Runtime, name string) error {
	if err := rt.StopWorkload(ctx, name, 0); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return err
	}
	return DeleteWorkload(ctx, rt, name)
}

// DeleteWorkload deletes a container for good, with the anonymous volumes the runtime made for its
// image's VOLUME paths (nothing else can use them); a missing one is fine.
func DeleteWorkload(ctx context.Context, rt ports.Runtime, name string) error {
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

// ProxyControl writes karga's proxies' configuration and reads their state.
type ProxyControl = ports.ProxyControl

// Host is what karga's proxies and probes need from the host: the proxies' control, the host's
// network, and whether the runtime can run the proxies. karga-server makes one and passes it down.
type Host struct {
	Proxies ProxyControl
	Net     ports.HostNet
	// supported says whether the runtime can run karga's proxies: they rely on bind-mounted unix
	// sockets, inotify and (the gateway) the host network namespace (a local, rootful Docker on Linux).
	supported atomic.Bool
}

// UseRuntime records whether the runtime described by info can run karga's proxies.
func (h *Host) UseRuntime(info ports.RuntimeInfo) { h.supported.Store(info.Has(ports.CapHostGateway)) }

// Supported reports whether the runtime can run karga's proxies.
func (h *Host) Supported() bool { return h.supported.Load() }

// ProxyReadyTimeout bounds how long a proxy may take to route to a new backend.
const ProxyReadyTimeout = 30 * time.Second

// HTTPHealthPath is the path Envoy checks on port when the workload's health check is http there.
func HTTPHealthPath(w *resource.WorkloadSpec, port int) string {
	if hc := w.HealthCheck; hc != nil && hc.Type == resource.HealthHTTP && hc.Port == port {
		return hc.Path
	}
	return ""
}
