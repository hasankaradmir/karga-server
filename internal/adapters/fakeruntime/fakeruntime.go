// Package fakeruntime is an in-memory ports.Runtime for unit tests.
package fakeruntime

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	pathpkg "path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Runtime is safe for concurrent use. Set Fail["<Op>:<name>"] to make an operation fail.
type Runtime struct {
	mu        sync.Mutex
	Images    map[string]bool
	Networks  map[string]ports.NetworkRequest
	Volumes   map[string]ports.VolumeRequest
	Workloads map[string]*Workload
	// Fail injects errors, e.g. Fail["StartWorkload:web-redis"] = errors.New("boom").
	Fail map[string]error
	// Calls records every operation as "<Op>:<name>" in order.
	Calls []string
	// Health is the health a started workload with a healthcheck reports (default healthy).
	Health string
	// Capabilities overrides the reported capabilities when non-nil.
	Capabilities []string
	// Output is what Logs returns for a workload.
	Output map[string]string
	// Files holds file contents by workload and absolute path, for ExportPath and ImportPath.
	Files map[string]map[string][]byte
	// ExecFunc answers Exec (default: no output, exit code 0).
	ExecFunc func(name string, opts ports.ExecOptions) int
	// ImageVolumes are the VOLUME paths of an image: a container gets an anonymous volume (named
	// "anon-<n>" in Volumes) for each one no mount covers.
	ImageVolumes map[string][]string
	anonymous    int
	// IPs are the network addresses GetWorkload reports for a workload (network -> address).
	IPs map[string]map[string]string
	// EventsC, if set, is what Events streams; tests send to it.
	EventsC chan ports.RuntimeEvent
}

// Workload is a fake container. Aliases maps each attached network to the workload's DNS aliases.
type Workload struct {
	Request ports.WorkloadRequest
	State   string
	Aliases map[string][]string
	// OOMKilled and Restarts are reported by GetWorkload; tests set them.
	OOMKilled bool
	Restarts  int
}

// New returns an empty runtime. Images listed in pulled are present locally.
func New(pulled ...string) *Runtime {
	r := &Runtime{
		Images:    map[string]bool{},
		Networks:  map[string]ports.NetworkRequest{},
		Volumes:   map[string]ports.VolumeRequest{},
		Workloads: map[string]*Workload{},
		Fail:      map[string]error{},
	}
	for _, i := range pulled {
		r.Images[i] = true
	}
	return r
}

func (r *Runtime) call(op, name string) error {
	key := op + ":" + name
	r.Calls = append(r.Calls, key)
	return r.Fail[key]
}

// WithGateway makes the runtime report the default capabilities plus karga's gateway, as Docker
// on Linux does. Use it with Proxies, which stands in for the Envoys' admin API.
func (r *Runtime) WithGateway() *Runtime {
	r.Capabilities = []string{
		ports.CapNetworkDriver("bridge"), ports.CapVolumeDriver("local"),
		ports.CapResourcesMemory, ports.CapResourcesCPU, ports.CapHealthcheckExec, ports.CapHostGateway,
	}
	return r
}

// Proxies writes karga's Envoy configuration files like the real thing and reports every proxy
// ready and drained at once.
type Proxies struct{}

// Write writes an Envoy's configuration.
func (Proxies) Write(dir, node string, mode int, cfg ports.ProxyConfig) error {
	return envoy.Write(dir, node, mode, cfg)
}

// WaitReady returns at once.
func (Proxies) WaitReady(context.Context, string, map[string]string, time.Duration) error { return nil }

// WaitDrained returns at once.
func (Proxies) WaitDrained(context.Context, string, []string, time.Duration) (bool, error) {
	return true, nil
}

// Container is Envoy's.
func (Proxies) Container() ports.ProxyContainer { return envoy.Proxies{}.Container() }

// Endpoints reads the configuration Write wrote.
func (Proxies) Endpoints(dir string) (map[string]string, error) { return envoy.Endpoints(dir) }

// Live reports every proxy live.
func (Proxies) Live(context.Context, string) (bool, error) { return true, nil }

// Version is a fixed version.
func (Proxies) Version(context.Context, string) (string, error) { return "fake", nil }

var _ ports.ProxyControl = Proxies{}

// Info implements ports.Runtime.
func (r *Runtime) Info(context.Context) (ports.RuntimeInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("Info", ""); err != nil {
		return ports.RuntimeInfo{}, err
	}
	caps := r.Capabilities
	if caps == nil {
		caps = []string{
			ports.CapNetworkDriver("bridge"), ports.CapVolumeDriver("local"),
			ports.CapResourcesMemory, ports.CapResourcesCPU, ports.CapHealthcheckExec,
		}
	}
	return ports.RuntimeInfo{Name: "docker", Version: "fake", Capabilities: caps}, nil
}

// EnsureImage implements ports.Runtime. Images under "missing/" or "karga.invalid/" cannot be pulled.
func (r *Runtime) EnsureImage(_ context.Context, ref, policy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("EnsureImage", ref); err != nil {
		return err
	}
	if policy == "if-not-present" && r.Images[ref] {
		return nil
	}
	if policy == "never" {
		if r.Images[ref] {
			return nil
		}
		return fmt.Errorf("image %s is not present and pullPolicy is never: %w", ref, ports.ErrImageNotFound)
	}
	if strings.HasPrefix(ref, "missing/") || strings.HasPrefix(ref, "karga.invalid/") {
		return fmt.Errorf("pull %s: %w", ref, ports.ErrImageNotFound)
	}
	r.Calls = append(r.Calls, "Pull:"+ref)
	r.Images[ref] = true
	return nil
}

// CreateNetwork implements ports.Runtime.
func (r *Runtime) CreateNetwork(_ context.Context, req ports.NetworkRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateNetwork", req.Name); err != nil {
		return err
	}
	if _, ok := r.Networks[req.Name]; ok {
		return fmt.Errorf("network %s: %w", req.Name, ports.ErrConflict)
	}
	r.Networks[req.Name] = req
	return nil
}

// DeleteNetwork implements ports.Runtime.
func (r *Runtime) DeleteNetwork(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteNetwork", name); err != nil {
		return err
	}
	delete(r.Networks, name)
	return nil
}

// CreateVolume implements ports.Runtime.
func (r *Runtime) CreateVolume(_ context.Context, req ports.VolumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateVolume", req.Name); err != nil {
		return err
	}
	if _, ok := r.Volumes[req.Name]; ok {
		return fmt.Errorf("volume %s: %w", req.Name, ports.ErrConflict)
	}
	r.Volumes[req.Name] = req
	return nil
}

// DeleteVolume implements ports.Runtime.
func (r *Runtime) DeleteVolume(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteVolume", name); err != nil {
		return err
	}
	delete(r.Volumes, name)
	return nil
}

// CreateWorkload implements ports.Runtime.
func (r *Runtime) CreateWorkload(_ context.Context, req ports.WorkloadRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("CreateWorkload", req.Name); err != nil {
		return err
	}
	if _, ok := r.Workloads[req.Name]; ok {
		return fmt.Errorf("workload %s: %w", req.Name, ports.ErrConflict)
	}
	if !r.Images[req.Image] {
		return fmt.Errorf("image %s: %w", req.Image, ports.ErrImageNotFound)
	}
	for _, n := range req.Networks {
		if _, ok := r.Networks[n]; !ok {
			return fmt.Errorf("network %s: %w", n, ports.ErrNotFound)
		}
	}
	w := &Workload{Request: req, State: ports.StateCreated, Aliases: map[string][]string{}}
	for _, n := range req.Networks {
		w.Aliases[n] = slices.Clone(req.Aliases)
	}
	for _, path := range r.ImageVolumes[req.Image] {
		if !slices.ContainsFunc(req.Mounts, func(m ports.Mount) bool { return m.Target == path }) {
			r.anonymous++
			name := fmt.Sprintf("anon-%d", r.anonymous)
			r.Volumes[name] = ports.VolumeRequest{Name: name}
			w.Request.Mounts = append(slices.Clone(w.Request.Mounts), ports.Mount{Type: ports.MountVolume, Source: name, Target: path})
		}
	}
	r.Workloads[req.Name] = w
	return nil
}

// StartWorkload implements ports.Runtime.
func (r *Runtime) StartWorkload(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("StartWorkload", name); err != nil {
		return err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	for _, p := range w.Request.Ports {
		if p.HostPort == 0 {
			continue
		}
		for otherName, o := range r.Workloads {
			if otherName == name || o.State != ports.StateRunning {
				continue
			}
			for _, op := range o.Request.Ports {
				if op.HostPort == p.HostPort && op.Protocol == p.Protocol {
					return fmt.Errorf("port %d/%s: %w", p.HostPort, p.Protocol, ports.ErrPortInUse)
				}
			}
		}
	}
	w.State = ports.StateRunning
	return nil
}

// StopWorkload implements ports.Runtime.
func (r *Runtime) StopWorkload(_ context.Context, name string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("StopWorkload", name); err != nil {
		return err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	w.State = ports.StateExited
	return nil
}

// DeleteWorkload implements ports.Runtime.
func (r *Runtime) DeleteWorkload(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DeleteWorkload", name); err != nil {
		return err
	}
	delete(r.Workloads, name)
	return nil
}

// GetWorkload implements ports.Runtime.
func (r *Runtime) GetWorkload(_ context.Context, name string) (ports.WorkloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Fail["GetWorkload:"+name]; err != nil {
		return ports.WorkloadStatus{}, err
	}
	w, ok := r.Workloads[name]
	if !ok {
		return ports.WorkloadStatus{}, fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	health := ports.HealthNone
	if w.Request.HealthCheck != nil && w.State == ports.StateRunning {
		health = ports.HealthHealthy
		if r.Health != "" {
			health = r.Health
		}
	}
	lim := ports.Limits{MemoryBytes: w.Request.MemoryBytes, NanoCPUs: w.Request.MilliCPU * 1_000_000, Pids: w.Request.PidsLimit,
		LogMaxSize: w.Request.LogMaxSize, LogMaxFiles: w.Request.LogMaxFiles,
		NoNewPrivileges: w.Request.NoNewPrivileges, ReadOnlyRootfs: w.Request.ReadOnlyRootfs}
	if lim.MemoryBytes > 0 {
		lim.MemorySwapBytes = lim.MemoryBytes
	}
	var anon []ports.Mount
	for _, m := range w.Request.Mounts {
		if m.Type == ports.MountVolume && strings.HasPrefix(m.Source, "anon-") {
			anon = append(anon, m)
		}
	}
	return ports.WorkloadStatus{Name: name, State: w.State, Health: health, Labels: maps.Clone(w.Request.Labels), Limits: lim,
		OOMKilled: w.OOMKilled, RestartCount: w.Restarts, AnonymousVolumes: anon, IPs: maps.Clone(r.IPs[name])}, nil
}

// ListManaged implements ports.Runtime.
func (r *Runtime) ListManaged(_ context.Context, release string) ([]ports.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.Object
	add := func(kind, name string, labels map[string]string) {
		if labels[ports.LabelManaged] == "true" && labels[ports.LabelRelease] == release {
			out = append(out, ports.Object{Kind: kind, Name: name, Labels: maps.Clone(labels)})
		}
	}
	for n, w := range r.Workloads {
		add(ports.ObjectWorkload, n, w.Request.Labels)
	}
	for n, v := range r.Networks {
		add(ports.ObjectNetwork, n, v.Labels)
	}
	for n, v := range r.Volumes {
		add(ports.ObjectVolume, n, v.Labels)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].Name < out[j].Kind+out[j].Name })
	return out, nil
}

// Logs implements ports.Runtime: it writes the last tail lines of Output[name] (follow ends at once).
func (r *Runtime) Logs(_ context.Context, name string, tail int, _ bool, w io.Writer) error {
	r.mu.Lock()
	out := r.Output[name]
	_, exists := r.Workloads[name]
	r.mu.Unlock()
	if !exists {
		return fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	}
	lines := strings.SplitAfter(out, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if tail >= 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	_, err := io.WriteString(w, strings.Join(lines, ""))
	return err
}

// Exec implements ports.Runtime.
func (r *Runtime) Exec(_ context.Context, name string, opts ports.ExecOptions) (int, error) {
	r.mu.Lock()
	err := r.call("Exec", name)
	w, ok := r.Workloads[name]
	fn := r.ExecFunc
	r.mu.Unlock()
	switch {
	case err != nil:
		return -1, err
	case !ok:
		return -1, fmt.Errorf("workload %s: %w", name, ports.ErrNotFound)
	case w.State != ports.StateRunning:
		return -1, fmt.Errorf("workload %s is not running: %w", name, ports.ErrConflict)
	case fn == nil:
		return 0, nil
	}
	return fn(name, opts), nil
}

// ExportPath implements ports.Runtime: files below path, as tar entries prefixed with its base name.
func (r *Runtime) ExportPath(_ context.Context, workload, path string, w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("ExportPath", workload); err != nil {
		return err
	}
	if _, ok := r.Workloads[workload]; !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	path = strings.TrimSuffix(path, "/")
	tw := tar.NewWriter(w)
	for _, name := range slices.Sorted(maps.Keys(r.Files[workload])) {
		rel, ok := strings.CutPrefix(name, path+"/")
		if !ok {
			continue
		}
		data := r.Files[workload][name]
		if err := tw.WriteHeader(&tar.Header{Name: pathpkg.Base(path) + "/" + rel, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// ImportPath implements ports.Runtime: tar entries become files below dir.
func (r *Runtime) ImportPath(_ context.Context, workload, dir string, rd io.Reader) error {
	tr := tar.NewReader(rd)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		files[pathpkg.Join(dir, h.Name)] = data
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("ImportPath", workload); err != nil {
		return err
	}
	if _, ok := r.Workloads[workload]; !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	if r.Files == nil {
		r.Files = map[string]map[string][]byte{}
	}
	if r.Files[workload] == nil {
		r.Files[workload] = map[string][]byte{}
	}
	maps.Copy(r.Files[workload], files)
	return nil
}

// ListLabeled implements ports.Runtime.
func (r *Runtime) ListLabeled(_ context.Context, key string) ([]ports.Object, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.Object
	for n, w := range r.Workloads {
		if _, ok := w.Request.Labels[key]; ok && w.Request.Labels[ports.LabelManaged] == "true" {
			out = append(out, ports.Object{Kind: ports.ObjectWorkload, Name: n, Labels: maps.Clone(w.Request.Labels)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PublishedPorts implements ports.Runtime.
func (r *Runtime) PublishedPorts(context.Context) ([]ports.PublishedPort, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.PublishedPort
	for n, w := range r.Workloads {
		if w.State != ports.StateRunning {
			continue
		}
		for _, p := range w.Request.Ports {
			if p.HostPort != 0 {
				out = append(out, ports.PublishedPort{
					HostIP: p.HostIP, Port: p.HostPort, Protocol: p.Protocol,
					Release: w.Request.Labels[ports.LabelRelease], Workload: n,
				})
			}
		}
	}
	return out, nil
}

// RenameWorkload implements ports.Runtime.
func (r *Runtime) RenameWorkload(_ context.Context, from, to string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("RenameWorkload", from+"->"+to); err != nil {
		return err
	}
	w, ok := r.Workloads[from]
	if !ok {
		return fmt.Errorf("workload %s: %w", from, ports.ErrNotFound)
	}
	if _, taken := r.Workloads[to]; taken {
		return fmt.Errorf("workload %s: %w", to, ports.ErrConflict)
	}
	delete(r.Workloads, from)
	w.Request.Name = to
	r.Workloads[to] = w
	return nil
}

// ConnectNetwork implements ports.Runtime.
func (r *Runtime) ConnectNetwork(_ context.Context, workload, network string, aliases []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("ConnectNetwork", workload+"@"+network+"="+strings.Join(aliases, ",")); err != nil {
		return err
	}
	w, ok := r.Workloads[workload]
	if !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	if _, ok := r.Networks[network]; !ok {
		return fmt.Errorf("network %s: %w", network, ports.ErrNotFound)
	}
	if _, attached := w.Aliases[network]; attached {
		return fmt.Errorf("workload %s already on network %s: %w", workload, network, ports.ErrConflict)
	}
	w.Aliases[network] = slices.Clone(aliases)
	return nil
}

// DisconnectNetwork implements ports.Runtime.
func (r *Runtime) DisconnectNetwork(_ context.Context, workload, network string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.call("DisconnectNetwork", workload+"@"+network); err != nil {
		return err
	}
	w, ok := r.Workloads[workload]
	if !ok {
		return fmt.Errorf("workload %s: %w", workload, ports.ErrNotFound)
	}
	delete(w.Aliases, network)
	return nil
}

// Events implements ports.Runtime: it streams EventsC (nothing when nil) until ctx is done.
func (r *Runtime) Events(ctx context.Context) (<-chan ports.RuntimeEvent, error) {
	out := make(chan ports.RuntimeEvent)
	r.mu.Lock()
	in := r.EventsC
	r.mu.Unlock()
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
