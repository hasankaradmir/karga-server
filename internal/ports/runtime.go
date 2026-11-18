// Package ports defines the boundaries karga's application layer talks to.
package ports

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"
)

// Errors every Runtime implementation wraps (with %w) so callers can react to them.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("already exists")
	ErrPortInUse     = errors.New("port already in use")
	ErrImageNotFound = errors.New("image not found")
	ErrUnavailable   = errors.New("runtime unavailable")
	ErrPermission    = errors.New("permission denied")
	// ErrInvalid marks a request that cannot be done as asked (a package that does not render,
	// settings that do not validate): the caller must change it.
	ErrInvalid = errors.New("invalid")
)

// Capability strings reported in RuntimeInfo.
const (
	CapResourcesMemory = "resources.memory"
	CapResourcesCPU    = "resources.cpu"
	CapHealthcheckExec = "healthcheck.exec"
	// CapHostGateway: karga can run its Envoy gateway and release proxies (ADR-010): a Linux host
	// whose Docker daemon is local, so bind mounts, unix sockets and the host network are shared.
	CapHostGateway = "karga.host-gateway"
)

// CapNetworkDriver and CapVolumeDriver build driver capability strings.
func CapNetworkDriver(d string) string { return "network.driver/" + d }

// CapVolumeDriver returns the capability string for a volume driver.
func CapVolumeDriver(d string) string { return "volume.driver/" + d }

// Labels karga puts on every runtime object. ListManaged selects on LabelManaged and LabelRelease.
const (
	LabelManaged   = "io.karga.managed"
	LabelRelease   = "io.karga.release"
	LabelKind      = "io.karga.kind"
	LabelName      = "io.karga.name"
	LabelRevision  = "io.karga.revision"
	LabelRetention = "io.karga.retention"
	// LabelDigest on a workload container is the digest of the spec it was created from, so a
	// container changed outside karga is told apart.
	LabelDigest = "io.karga.digest"
)

// Kinds returned by ListManaged.
const (
	ObjectWorkload = "workload"
	ObjectNetwork  = "network"
	ObjectVolume   = "volume"
)

// Runtime is a container runtime (Docker, later Podman). Objects are addressed by name.
// Create returns ErrConflict if the name exists; Delete of a missing object succeeds.
type Runtime interface {
	Info(ctx context.Context) (RuntimeInfo, error)
	// EnsureImage makes ref available according to policy (always, if-not-present, never).
	EnsureImage(ctx context.Context, ref, policy string) error

	CreateNetwork(ctx context.Context, req NetworkRequest) error
	DeleteNetwork(ctx context.Context, name string) error
	CreateVolume(ctx context.Context, req VolumeRequest) error
	DeleteVolume(ctx context.Context, name string) error

	// CreateWorkload creates a container without starting it.
	CreateWorkload(ctx context.Context, req WorkloadRequest) error
	StartWorkload(ctx context.Context, name string) error
	// StopWorkload stops a container: SIGTERM, then SIGKILL after timeout. A timeout of 0 uses the
	// container's own StopTimeout (the runtime default when it has none).
	StopWorkload(ctx context.Context, name string, timeout time.Duration) error
	// DeleteWorkload removes the container (stopping it if needed). Its volumes, named and anonymous,
	// are kept.
	DeleteWorkload(ctx context.Context, name string) error
	GetWorkload(ctx context.Context, name string) (WorkloadStatus, error)
	// RenameWorkload renames a container; it keeps running and keeps its network aliases.
	RenameWorkload(ctx context.Context, from, to string) error
	// ConnectNetwork attaches a workload to a network with the given DNS aliases.
	ConnectNetwork(ctx context.Context, workload, network string, aliases []string) error
	// DisconnectNetwork detaches a workload; it succeeds if the workload is not attached.
	DisconnectNetwork(ctx context.Context, workload, network string) error

	// ListManaged returns every karga-managed object of a release.
	ListManaged(ctx context.Context, release string) ([]Object, error)
	// Logs writes the last tail lines (all with tail < 0) a workload wrote to stdout and stderr to w.
	// With follow it keeps writing new lines until ctx is done.
	Logs(ctx context.Context, name string, tail int, follow bool, w io.Writer) error
	// Exec runs a command in a running workload and returns its exit code.
	Exec(ctx context.Context, name string, opts ExecOptions) (int, error)
	// ExportPath writes path of a workload (running or stopped) as a tar stream whose entries are
	// prefixed with the base name of path.
	ExportPath(ctx context.Context, workload, path string, w io.Writer) error
	// ImportPath extracts a tar stream into dir of a workload, keeping owners and modes.
	ImportPath(ctx context.Context, workload, dir string, r io.Reader) error
	// ListLabeled returns every workload, of any release, that carries label key.
	ListLabeled(ctx context.Context, key string) ([]Object, error)
	// PublishedPorts returns host ports published by any container.
	PublishedPorts(ctx context.Context) ([]PublishedPort, error)
	// Events streams what happens to karga-managed containers until ctx is done or the stream
	// breaks; then the channel is closed.
	Events(ctx context.Context) (<-chan RuntimeEvent, error)
}

// RuntimeEvent is something that happened to a karga-managed container: Action is die, oom,
// destroy, stop, start or unhealthy.
type RuntimeEvent struct {
	Release   string
	Container string
	Action    string
}

// RuntimeInfo describes the runtime and what it supports.
type RuntimeInfo struct {
	Name         string
	Version      string
	Capabilities []string
}

// Has reports whether the runtime has capability c.
func (i RuntimeInfo) Has(c string) bool { return slices.Contains(i.Capabilities, c) }

// NetworkRequest creates a network.
type NetworkRequest struct {
	Name     string
	Labels   map[string]string
	Driver   string
	Internal bool
}

// VolumeRequest creates a named volume.
type VolumeRequest struct {
	Name   string
	Labels map[string]string
	Driver string
}

// WorkloadRequest creates a container.
type WorkloadRequest struct {
	Name    string
	Labels  map[string]string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// User overrides the image user ("uid[:gid]"). Empty keeps the image default.
	User string
	// Sysctls are namespaced kernel parameters for the container (e.g. net.ipv4.ip_unprivileged_port_start).
	Sysctls map[string]string
	// HostNetwork runs the container in the host's network namespace; Networks and Ports must be empty.
	HostNetwork bool
	// CapAdd and CapDrop change the container's Linux capabilities ("ALL" drops every one).
	CapAdd, CapDrop []string
	// NoNewPrivileges blocks privilege gains through setuid binaries.
	NoNewPrivileges bool
	// ReadOnlyRootfs mounts the image read-only; Tmpfs adds in-memory mounts (path -> options).
	ReadOnlyRootfs bool
	Tmpfs          map[string]string
	Ports          []PortBinding
	Mounts         []Mount
	Networks       []string
	Aliases        []string
	HealthCheck    *HealthCheck
	MemoryBytes    int64
	MilliCPU       int64
	// PidsLimit caps processes (0: no limit). With MemoryBytes, no swap is allowed beyond it.
	PidsLimit int64
	// LogMaxSize and LogMaxFiles rotate the container log (empty: runtime default).
	LogMaxSize    string
	LogMaxFiles   int
	RestartPolicy string
	// StopTimeout is how long a stop waits after SIGTERM before killing the container (0: runtime default).
	StopTimeout time.Duration
}

// PortBinding publishes ContainerPort. HostPort 0 means the port is not published.
type PortBinding struct {
	ContainerPort int
	HostPort      int
	HostIP        string
	Protocol      string
}

// Mount types.
const (
	MountVolume = "volume"
	MountBind   = "bind"
)

// Mount attaches a named volume or a host path.
type Mount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

// HealthCheck runs Command inside the container.
type HealthCheck struct {
	Command     []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// Workload states and health values.
const (
	StateCreated = "created"
	StateRunning = "running"
	StateExited  = "exited"

	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// WorkloadStatus is the observed state of a container.
type WorkloadStatus struct {
	Name     string
	State    string
	Health   string
	ExitCode int
	Labels   map[string]string
	// OOMKilled is true when the last run ended by exceeding its memory limit.
	OOMKilled    bool
	RestartCount int
	// Limits is what the runtime enforces, as it reports it.
	Limits Limits
	// IPs maps each attached network to the workload's address on it.
	IPs map[string]string
	// AnonymousVolumes are the volumes the runtime created for image VOLUME paths that no mount
	// covers (Type MountVolume, Source the generated volume name, Target the path).
	AnonymousVolumes []Mount
}

// ExecOptions is shared with the CLI (karga-sdk/api).
type ExecOptions struct {
	Cmd            []string
	TTY            bool
	Stdin          io.Reader // nil: no input
	Stdout, Stderr io.Writer
	Width, Height  uint
	Resize         <-chan [2]uint // width, height
}

// Limits are the enforced resource and logging limits of a workload.
type Limits struct {
	MemoryBytes, MemorySwapBytes, NanoCPUs, Pids int64
	LogMaxSize                                   string
	LogMaxFiles                                  int
	NoNewPrivileges, ReadOnlyRootfs              bool
}

// Object is a managed runtime object.
type Object struct {
	Kind   string
	Name   string
	Labels map[string]string
}

// PublishedPort is a host port bound by a container.
type PublishedPort struct {
	HostIP   string
	Port     int
	Protocol string
	// Release is the owning karga release, empty for containers karga does not manage.
	Release string
	// Workload is the container name.
	Workload string
}
