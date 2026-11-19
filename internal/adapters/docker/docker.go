// Package docker implements ports.Runtime on the Docker Engine API.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Runtime talks to one Docker Engine. It honours DOCKER_HOST and related variables.
type Runtime struct {
	c *client.Client
}

var _ ports.Runtime = (*Runtime)(nil)

// New connects to Docker (DOCKER_HOST or the active docker context) and checks the daemon answers.
func New(ctx context.Context) (*Runtime, error) {
	opts := []client.Opt{client.FromEnv}
	host, contextName, err := contextHost(os.Getenv)
	if err != nil {
		return nil, err
	}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		_ = c.Close()
		return nil, unreachable(c.DaemonHost(), contextName, err)
	}
	return &Runtime{c: c}, nil
}

// Close releases the client connection.
func (r *Runtime) Close() error { return r.c.Close() }

// Deadlines of Docker calls: a daemon that stops answering must not hold an operation, and the
// release it locks, for ever. Streams (logs, exec, copies, events) are not bounded.
const (
	callTimeout = 2 * time.Minute
	pullTimeout = 30 * time.Minute
)

// unreachable explains a failed connection in terms of what the user can change.
func unreachable(host, contextName string, err error) error {
	where := host
	if contextName != "" {
		where += fmt.Sprintf(" (docker context %q)", contextName)
	}
	cause := err
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		cause = opErr.Err
	}
	switch {
	case strings.Contains(err.Error(), "permission denied"):
		return fmt.Errorf("permission denied on the Docker socket %s; add your user to the docker group or use a rootless daemon: %w", where, ports.ErrPermission)
	case client.IsErrConnectionFailed(err):
		return fmt.Errorf("cannot reach Docker at %s (%v); is the daemon running? Use DOCKER_HOST or 'docker context use' to choose another daemon: %w", where, cause, ports.ErrUnavailable)
	default:
		return wrap(err, "connect to Docker at "+where)
	}
}

// wrap maps Docker errors to the ports sentinel errors and adds what failed.
func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		return fmt.Errorf("%s: %w (is your user allowed to use the Docker socket?): %w", what, err, ports.ErrPermission)
	case client.IsErrConnectionFailed(err):
		return fmt.Errorf("%s: %w (is the Docker daemon running?): %w", what, err, ports.ErrUnavailable)
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%s: %w", what, ports.ErrNotFound)
	case cerrdefs.IsConflict(err) || cerrdefs.IsAlreadyExists(err):
		return fmt.Errorf("%s: %w", what, ports.ErrConflict)
	case strings.Contains(msg, "port is already allocated") || strings.Contains(msg, "address already in use"):
		return fmt.Errorf("%s: %w: %w", what, err, ports.ErrPortInUse)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// ignoreNotFound makes deletes idempotent.
func ignoreNotFound(err error) error {
	if errors.Is(err, ports.ErrNotFound) {
		return nil
	}
	return err
}

// Info implements ports.Runtime.
func (r *Runtime) Info(ctx context.Context) (ports.RuntimeInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := r.c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return ports.RuntimeInfo{}, wrap(err, "docker info")
	}
	info := ports.RuntimeInfo{Name: "docker", Version: res.Info.ServerVersion}
	for _, d := range res.Info.Plugins.Network {
		info.Capabilities = append(info.Capabilities, ports.CapNetworkDriver(d))
	}
	for _, d := range res.Info.Plugins.Volume {
		info.Capabilities = append(info.Capabilities, ports.CapVolumeDriver(d))
	}
	if res.Info.MemoryLimit {
		info.Capabilities = append(info.Capabilities, ports.CapResourcesMemory)
	}
	if res.Info.CPUCfsQuota {
		info.Capabilities = append(info.Capabilities, ports.CapResourcesCPU)
	}
	info.Capabilities = append(info.Capabilities, ports.CapHealthcheckExec)
	// The gateway binds the host's ports in the host network namespace and the probes reach
	// containers by address: a local, rootful daemon on Linux. A rootless daemon has its own network
	// namespace, where neither works.
	if goruntime.GOOS == "linux" && strings.HasPrefix(r.c.DaemonHost(), "unix://") && !rootless(res.Info.SecurityOptions) {
		info.Capabilities = append(info.Capabilities, ports.CapHostGateway)
	}
	return info, nil
}

// CreateNetwork implements ports.Runtime.
func (r *Runtime) CreateNetwork(ctx context.Context, req ports.NetworkRequest) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	opts := client.NetworkCreateOptions{Driver: req.Driver, Internal: req.Internal, Labels: req.Labels, Options: req.Options}
	if req.Subnet != "" {
		subnet, err := netip.ParsePrefix(req.Subnet)
		if err != nil {
			return fmt.Errorf("create network %s: subnet %q: %w", req.Name, req.Subnet, ports.ErrInvalid)
		}
		opts.IPAM = &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}}
	}
	_, err := r.c.NetworkCreate(ctx, req.Name, opts)
	return wrap(err, "create network "+req.Name)
}

// DeleteNetwork implements ports.Runtime.
func (r *Runtime) DeleteNetwork(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := r.c.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	return ignoreNotFound(wrap(err, "delete network "+name))
}

// CreateVolume implements ports.Runtime. Docker returns an existing volume on create, so check first.
func (r *Runtime) CreateVolume(ctx context.Context, req ports.VolumeRequest) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := r.c.VolumeInspect(ctx, req.Name, client.VolumeInspectOptions{}); err == nil {
		return fmt.Errorf("create volume %s: %w", req.Name, ports.ErrConflict)
	} else if !cerrdefs.IsNotFound(err) {
		return wrap(err, "inspect volume "+req.Name)
	}
	_, err := r.c.VolumeCreate(ctx, client.VolumeCreateOptions{Name: req.Name, Driver: req.Driver, Labels: req.Labels})
	return wrap(err, "create volume "+req.Name)
}

// DeleteVolume implements ports.Runtime.
func (r *Runtime) DeleteVolume(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := r.c.VolumeRemove(ctx, name, client.VolumeRemoveOptions{})
	return ignoreNotFound(wrap(err, "delete volume "+name))
}

// rootless reports whether the daemon's security options say it runs rootless.
func rootless(opts []string) bool {
	for _, o := range opts {
		if strings.Contains(o, "name=rootless") {
			return true
		}
	}
	return false
}
