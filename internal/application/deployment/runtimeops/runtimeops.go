// Package runtimeops holds what the deployment executor, the release proxy and the host gateway
// share: runtime object names and removal, host port checks, and control of karga's Envoys.
package runtimeops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
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

// PortTaken test-binds a host port: taken when another process holds it. err says the port could
// not be tested: karga-server runs without privileges, and binding a port below 1024 needs them
// (Docker binds the gateway's ports as root).
func PortTaken(protocol, ip string, port int) (taken bool, err error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	if protocol == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			_ = c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			_ = l.Close()
		}
	}
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return true, nil
	case errors.Is(err, syscall.EACCES):
		return false, err
	}
	return false, nil
}

// Supported is set by UseProxies: karga's proxies rely on bind-mounted unix sockets, inotify
// and (the gateway) the host network namespace, so only a local Docker daemon on Linux has them.
var Supported atomic.Bool

// UseProxies turns the gateway and release proxies on when the runtime supports them. The
// release service calls it once it knows the runtime, before planning.
func UseProxies(info ports.RuntimeInfo) { Supported.Store(info.Has(ports.CapHostGateway)) }

// ProxyControl writes karga's proxies' configuration and reads their state.
type ProxyControl = ports.ProxyControl

// Proxies controls the release proxies and the gateway: Envoy, set by karga-server at start; tests
// substitute a fake. A request may name another (deployment.ExecRequest.Proxies).
var Proxies ProxyControl

// ProxyReadyTimeout bounds how long a proxy may take to route to a new backend.
const ProxyReadyTimeout = 30 * time.Second

// HTTPHealthPath is the path Envoy checks on port when the workload's health check is http there.
func HTTPHealthPath(w *resource.WorkloadSpec, port int) string {
	if hc := w.HealthCheck; hc != nil && hc.Type == resource.HealthHTTP && hc.Port == port {
		return hc.Path
	}
	return ""
}

// Limits of karga's Envoys: one worker uses about 15MB; the rest is headroom for connections.
const (
	EnvoyMemory   = 128 << 20
	EnvoyMilliCPU = 500
	EnvoyPids     = 256
)

// EnvoyLimits names the limits in what decides whether an Envoy container must be recreated.
var EnvoyLimits = fmt.Sprintf("mem=%d,cpu=%dm,pids=%d,log=%sx%d", EnvoyMemory, EnvoyMilliCPU, EnvoyPids,
	resource.DefaultLogMaxSize, resource.DefaultLogMaxFiles)
