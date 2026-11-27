// Package runtimetest is the contract every ports.Runtime implementation must pass.
package runtimetest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Config tells the suite which image and commands to use.
type Config struct {
	// ReportsIPs: GetWorkload returns the workload's network addresses.
	ReportsIPs bool
	// ExecCommand copies stdin to stdout, writes "to stderr" to stderr and exits 3; nil skips the check.
	ExecCommand []string
	// Image must provide SleepCommand and HealthyCommand.
	Image          string
	SleepCommand   []string
	HealthyCommand []string
	// Wait bounds how long the suite waits for a workload to become healthy.
	Wait time.Duration
	// VolumeImage declares VOLUME VolumePath; empty skips the anonymous volume check.
	VolumeImage, VolumePath string
}

// Run exercises rt against the Runtime contract.
func Run(t *testing.T, rt ports.Runtime, cfg Config) {
	t.Helper()
	if cfg.Wait == 0 {
		cfg.Wait = 60 * time.Second
	}
	s := &suite{rt: rt, cfg: cfg, ctx: context.Background(), release: "ct-" + randHex(4)}
	t.Run("info", s.info)
	t.Run("network", s.network)
	t.Run("volume", s.volume)
	t.Run("volume data", s.volumeData)
	t.Run("stop timeout", s.stopTimeout)
	t.Run("anonymous volume", s.anonymousVolume)
	t.Run("image never missing", s.imageNeverMissing)
	t.Run("missing workload", s.missingWorkload)
	t.Run("workload lifecycle", s.workloadLifecycle)
}

// suite is one run of the contract: the runtime, its configuration and the release its objects
// belong to.
type suite struct {
	rt      ports.Runtime
	cfg     Config
	ctx     context.Context
	release string
}

func (s *suite) labels(kind, name string) map[string]string {
	return map[string]string{
		ports.LabelManaged: "true", ports.LabelRelease: s.release,
		ports.LabelKind: kind, ports.LabelName: name, ports.LabelRevision: "1",
	}
}

func (s *suite) name(n string) string { return s.release + "-" + n }

func (s *suite) info(t *testing.T) {
	rt, ctx := s.rt, s.ctx
	info, err := rt.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name == "" || !info.Has(ports.CapHealthcheckExec) {
		t.Fatalf("unexpected info: %+v", info)
	}
}

func (s *suite) network(t *testing.T) {
	rt, ctx, release, labels, name := s.rt, s.ctx, s.release, s.labels, s.name
	req := ports.NetworkRequest{Name: name("net"), Labels: labels("Network", "net"), Driver: "bridge"}
	must(t, rt.CreateNetwork(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteNetwork(ctx, req.Name) })
	if err := rt.CreateNetwork(ctx, req); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate network: want ErrConflict, got %v", err)
	}
	assertListed(t, rt, release, ports.ObjectNetwork, req.Name, true)
	must(t, rt.DeleteNetwork(ctx, req.Name))
	must(t, rt.DeleteNetwork(ctx, req.Name))
	assertListed(t, rt, release, ports.ObjectNetwork, req.Name, false)
}

func (s *suite) volume(t *testing.T) {
	rt, ctx, release, labels, name := s.rt, s.ctx, s.release, s.labels, s.name
	req := ports.VolumeRequest{Name: name("vol"), Labels: labels("Volume", "vol"), Driver: "local"}
	must(t, rt.CreateVolume(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteVolume(ctx, req.Name) })
	if err := rt.CreateVolume(ctx, req); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate volume: want ErrConflict, got %v", err)
	}
	assertListed(t, rt, release, ports.ObjectVolume, req.Name, true)
	must(t, rt.DeleteVolume(ctx, req.Name))
	must(t, rt.DeleteVolume(ctx, req.Name))
}

// A volume's files outlive its containers, can be written through a container that was created
// but never started (even with a read-only image), and a volume a container mounts cannot be
// deleted. A restore that recreates a volume relies on all three.
func (s *suite) volumeData(t *testing.T) {
	rt, cfg, ctx, labels, name := s.rt, s.cfg, s.ctx, s.labels, s.name
	must(t, rt.EnsureImage(ctx, cfg.Image, "if-not-present", nil))
	vol := ports.VolumeRequest{Name: name("data"), Labels: labels("Volume", "data"), Driver: "local"}
	must(t, rt.CreateVolume(ctx, vol))
	t.Cleanup(func() { _ = rt.DeleteVolume(ctx, vol.Name) })
	req := ports.WorkloadRequest{Name: name("writer"), Labels: labels("Workload", "writer"), Image: cfg.Image, Command: cfg.SleepCommand,
		ReadOnlyRootfs: true, Mounts: []ports.Mount{{Type: ports.MountVolume, Source: vol.Name, Target: "/data"}}}
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	must(t, tw.WriteHeader(&tar.Header{Name: "kept.txt", Mode: 0o644, Size: 4}))
	_, _ = tw.Write([]byte("kept"))
	must(t, tw.Close())
	must(t, rt.ImportPath(ctx, req.Name, "/data", &in))
	if err := rt.DeleteVolume(ctx, vol.Name); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("deleting a mounted volume: want ErrConflict, got %v", err)
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))
	req.Name = name("reader")
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	var out bytes.Buffer
	must(t, rt.ExportPath(ctx, req.Name, "/data", &out))
	tr := tar.NewReader(&out)
	found := false
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		found = found || h.Name == "data/kept.txt"
	}
	if !found {
		t.Fatal("data/kept.txt not in the volume after its first container went")
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))
	must(t, rt.DeleteVolume(ctx, vol.Name))
}

func (s *suite) stopTimeout(t *testing.T) {
	rt, cfg, ctx, labels, name := s.rt, s.cfg, s.ctx, s.labels, s.name
	must(t, rt.EnsureImage(ctx, cfg.Image, "if-not-present", nil))
	// The sleep command ignores SIGTERM, so stopping takes the whole stop timeout.
	req := ports.WorkloadRequest{Name: name("stop"), Labels: labels("Workload", "stop"), Image: cfg.Image,
		Command: cfg.SleepCommand, StopTimeout: time.Second}
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	must(t, rt.StartWorkload(ctx, req.Name))
	start := time.Now()
	must(t, rt.StopWorkload(ctx, req.Name, 0))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stop with the workload's 1s StopTimeout took %s", d)
	}
}

func (s *suite) anonymousVolume(t *testing.T) {
	rt, cfg, ctx, labels, name := s.rt, s.cfg, s.ctx, s.labels, s.name
	if cfg.VolumeImage == "" {
		t.Skip("no VolumeImage")
	}
	must(t, rt.EnsureImage(ctx, cfg.VolumeImage, "if-not-present", nil))
	req := ports.WorkloadRequest{Name: name("anon"), Labels: labels("Workload", "anon"), Image: cfg.VolumeImage}
	must(t, rt.CreateWorkload(ctx, req))
	st, err := rt.GetWorkload(ctx, req.Name)
	must(t, err)
	if len(st.AnonymousVolumes) != 1 || st.AnonymousVolumes[0].Target != cfg.VolumePath {
		t.Fatalf("anonymous volumes: %+v", st.AnonymousVolumes)
	}
	vol := st.AnonymousVolumes[0].Source
	t.Cleanup(func() { _ = rt.DeleteVolume(ctx, vol) })
	// Deleting the container keeps the volume, so a new container can mount it.
	must(t, rt.DeleteWorkload(ctx, req.Name))
	req.Mounts = []ports.Mount{{Type: ports.MountVolume, Source: vol, Target: cfg.VolumePath}}
	must(t, rt.CreateWorkload(ctx, req))
	st, err = rt.GetWorkload(ctx, req.Name)
	must(t, err)
	if len(st.AnonymousVolumes) != 1 || st.AnonymousVolumes[0].Source != vol {
		t.Fatalf("after moving %s: %+v", vol, st.AnonymousVolumes)
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))
	must(t, rt.DeleteVolume(ctx, vol))
}

func (s *suite) imageNeverMissing(t *testing.T) {
	rt, ctx := s.rt, s.ctx
	if err := rt.EnsureImage(ctx, "karga.invalid/missing:0", "never", nil); !errors.Is(err, ports.ErrImageNotFound) {
		t.Fatalf("want ErrImageNotFound, got %v", err)
	}
}

func (s *suite) missingWorkload(t *testing.T) {
	rt, ctx, name := s.rt, s.ctx, s.name
	if _, err := rt.GetWorkload(ctx, name("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("GetWorkload: want ErrNotFound, got %v", err)
	}
	if err := rt.StartWorkload(ctx, name("nope")); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("StartWorkload: want ErrNotFound, got %v", err)
	}
	must(t, rt.DeleteWorkload(ctx, name("nope")))
}

func (s *suite) workloadLifecycle(t *testing.T) {
	rt, cfg, ctx, release, labels, name := s.rt, s.cfg, s.ctx, s.release, s.labels, s.name
	must(t, rt.EnsureImage(ctx, cfg.Image, "if-not-present", nil))
	netReq := ports.NetworkRequest{Name: name("wnet"), Labels: labels("Network", "wnet"), Driver: "bridge"}
	must(t, rt.CreateNetwork(ctx, netReq))
	t.Cleanup(func() { _ = rt.DeleteNetwork(ctx, netReq.Name) })

	port := freePort(t)
	req := ports.WorkloadRequest{
		Name:     name("app"),
		Labels:   labels("Workload", "app"),
		Image:    cfg.Image,
		Command:  cfg.SleepCommand,
		Env:      map[string]string{"KARGA_TEST": "1"},
		Ports:    []ports.PortBinding{{ContainerPort: 8080, HostPort: port, HostIP: "127.0.0.1", Protocol: "tcp"}},
		Networks: []string{netReq.Name},
		Aliases:  []string{"app"},
		HealthCheck: &ports.HealthCheck{
			Command: cfg.HealthyCommand, Interval: time.Second, Timeout: time.Second, Retries: 3,
		},
		MemoryBytes:     64 << 20,
		MilliCPU:        500,
		PidsLimit:       100,
		LogMaxSize:      "1m",
		LogMaxFiles:     2,
		Sysctls:         map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"},
		RestartPolicy:   "never",
		CapDrop:         []string{"ALL"},
		ReadOnlyRootfs:  true,
		Tmpfs:           map[string]string{"/tmp": "rw,size=1m"},
		CapAdd:          []string{"NET_BIND_SERVICE"},
		NoNewPrivileges: true,
	}
	req.Labels["io.karga.contract"] = "x"
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	if err := rt.CreateWorkload(ctx, req); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate workload: want ErrConflict, got %v", err)
	}
	st, err := rt.GetWorkload(ctx, req.Name)
	must(t, err)
	if st.State != ports.StateCreated {
		t.Fatalf("state after create: %q", st.State)
	}
	// The runtime enforces what was asked: memory without extra swap, CPU and processes.
	if want := (ports.Limits{MemoryBytes: 64 << 20, MemorySwapBytes: 64 << 20, NanoCPUs: 500_000_000, Pids: 100, LogMaxSize: "1m", LogMaxFiles: 2,
		NoNewPrivileges: true, ReadOnlyRootfs: true}); st.Limits != want {
		t.Fatalf("limits: %+v, want %+v", st.Limits, want)
	}

	must(t, rt.StartWorkload(ctx, req.Name))
	st = waitFor(t, rt, req.Name, cfg.Wait, func(s ports.WorkloadStatus) bool {
		return s.State == ports.StateRunning && s.Health == ports.HealthHealthy
	})
	if st.Labels[ports.LabelRelease] != release || st.Labels[ports.LabelName] != "app" {
		t.Fatalf("labels: %v", st.Labels)
	}
	if cfg.ReportsIPs && st.IPs[netReq.Name] == "" {
		t.Fatalf("no address on %s: %v", netReq.Name, st.IPs)
	}
	assertListed(t, rt, release, ports.ObjectWorkload, req.Name, true)
	var logBuf strings.Builder
	if err := rt.Logs(ctx, req.Name, 10, false, &logBuf); err != nil {
		t.Fatalf("logs: %v", err)
	}
	var execOut, execErr bytes.Buffer
	code, err := rt.Exec(ctx, req.Name, ports.ExecOptions{Cmd: cfg.ExecCommand, Stdin: strings.NewReader("from stdin"), Stdout: &execOut, Stderr: &execErr})
	must(t, err)
	if cfg.ExecCommand != nil && (code != 3 || !strings.Contains(execOut.String(), "from stdin") || !strings.Contains(execErr.String(), "to stderr")) {
		t.Fatalf("exec: code %d, stdout %q, stderr %q", code, execOut.String(), execErr.String())
	}
	labeled, err := rt.ListLabeled(ctx, "io.karga.contract")
	must(t, err)
	if !slices.ContainsFunc(labeled, func(o ports.Object) bool { return o.Name == req.Name && o.Labels["io.karga.contract"] == "x" }) {
		t.Fatalf("ListLabeled does not return %s: %+v", req.Name, labeled)
	}

	pub, err := rt.PublishedPorts(ctx)
	must(t, err)
	if !slices.ContainsFunc(pub, func(p ports.PublishedPort) bool {
		return p.Port == port && p.Protocol == "tcp" && p.Release == release && p.Workload == req.Name
	}) {
		t.Fatalf("port %d not reported in %+v", port, pub)
	}

	// Network membership can be changed while the container runs.
	must(t, rt.DisconnectNetwork(ctx, req.Name, netReq.Name))
	must(t, rt.DisconnectNetwork(ctx, req.Name, netReq.Name))
	must(t, rt.ConnectNetwork(ctx, req.Name, netReq.Name, []string{"app"}, ""))

	// A network with its own subnet and inter-container traffic off; a fixed address on it.
	edge := ports.NetworkRequest{Name: name("edge"), Labels: labels("Network", "edge"), Driver: "bridge", Internal: true,
		Subnet: "10.231." + strconv.Itoa(os.Getpid()%250) + ".0/24", Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"}}
	must(t, rt.CreateNetwork(ctx, edge))
	t.Cleanup(func() { _ = rt.DeleteNetwork(ctx, edge.Name) })
	ip := strings.TrimSuffix(edge.Subnet, "0/24") + "9"
	must(t, rt.ConnectNetwork(ctx, req.Name, edge.Name, nil, ip))
	if st, err := rt.GetWorkload(ctx, req.Name); err != nil || st.IPs[edge.Name] != ip {
		t.Fatalf("address on %s: %v, %v", edge.Name, st.IPs, err)
	}
	must(t, rt.DisconnectNetwork(ctx, req.Name, edge.Name))

	must(t, rt.StopWorkload(ctx, req.Name, 5*time.Second))
	if st, err := rt.GetWorkload(ctx, req.Name); err != nil || st.State != ports.StateExited {
		t.Fatalf("after stop: %+v, %v", st, err)
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))
	if _, err := rt.GetWorkload(ctx, req.Name); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))

	req.Name = name("hostnet")
	hostNetworkCase(t, rt, cfg, req)
	req.Name = name("copy")
	copyCase(t, rt, cfg, req)
	req.Name = name("invalid")
	invalidCase(t, rt, req)
}

// copyCase imports a file into a stopped workload and exports it again (volume backup's mechanism).
func copyCase(t *testing.T, rt ports.Runtime, cfg Config, req ports.WorkloadRequest) {
	ctx := context.Background()
	req.ReadOnlyRootfs, req.Tmpfs = false, nil
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	must(t, tw.WriteHeader(&tar.Header{Name: "karga-copy/hello.txt", Mode: 0o644, Size: 2}))
	_, _ = tw.Write([]byte("hi"))
	must(t, tw.Close())
	must(t, rt.ImportPath(ctx, req.Name, "/", &in))
	var out bytes.Buffer
	must(t, rt.ExportPath(ctx, req.Name, "/karga-copy", &out))
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("hello.txt not exported: %v", err)
		}
		if h.Name == "karga-copy/hello.txt" {
			data, _ := io.ReadAll(tr)
			if string(data) != "hi" {
				t.Fatalf("exported %q", data)
			}
			break
		}
	}
	must(t, rt.DeleteWorkload(ctx, req.Name))
}

// invalidCase: a runtime refuses an invalid request (WorkloadRequest.Validate) with ErrInvalid and
// creates nothing, so that the fake refuses what Docker refuses.
func invalidCase(t *testing.T, rt ports.Runtime, req ports.WorkloadRequest) {
	ctx := context.Background()
	for what, change := range map[string]func(*ports.WorkloadRequest){
		"unknown restart policy": func(r *ports.WorkloadRequest) { r.RestartPolicy = "sometimes" },
		"port without protocol":  func(r *ports.WorkloadRequest) { r.Ports = []ports.PortBinding{{ContainerPort: 80}} },
		"published without host IP": func(r *ports.WorkloadRequest) {
			r.Ports = []ports.PortBinding{{ContainerPort: 80, HostPort: 18080, Protocol: "tcp"}}
		},
		"host network and networks": func(r *ports.WorkloadRequest) { r.HostNetwork = true },
	} {
		r := req
		change(&r)
		if err := rt.CreateWorkload(ctx, r); !errors.Is(err, ports.ErrInvalid) {
			_ = rt.DeleteWorkload(ctx, r.Name)
			t.Errorf("%s: %v, want ErrInvalid", what, err)
			continue
		}
		if _, err := rt.GetWorkload(ctx, r.Name); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("%s: a workload was created: %v", what, err)
		}
	}
}

// hostNetworkCase runs a workload in the host network namespace (the gateway's mode).
func hostNetworkCase(t *testing.T, rt ports.Runtime, cfg Config, req ports.WorkloadRequest) {
	ctx := context.Background()
	req.HostNetwork, req.Networks, req.Aliases, req.Ports, req.HealthCheck, req.Sysctls = true, nil, nil, nil, nil, nil
	must(t, rt.CreateWorkload(ctx, req))
	t.Cleanup(func() { _ = rt.DeleteWorkload(ctx, req.Name) })
	must(t, rt.StartWorkload(ctx, req.Name))
	waitFor(t, rt, req.Name, cfg.Wait, func(s ports.WorkloadStatus) bool { return s.State == ports.StateRunning })
	must(t, rt.DeleteWorkload(ctx, req.Name))
}

func waitFor(t *testing.T, rt ports.Runtime, name string, d time.Duration, ok func(ports.WorkloadStatus) bool) ports.WorkloadStatus {
	t.Helper()
	deadline := time.Now().Add(d)
	var last ports.WorkloadStatus
	for time.Now().Before(deadline) {
		st, err := rt.GetWorkload(context.Background(), name)
		must(t, err)
		if ok(st) {
			return st
		}
		last = st
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("workload %s did not reach the wanted state in %s, last: %+v", name, d, last)
	return last
}

func assertListed(t *testing.T, rt ports.Runtime, release, kind, name string, want bool) {
	t.Helper()
	objs, err := rt.ListManaged(context.Background(), release)
	must(t, err)
	got := slices.ContainsFunc(objs, func(o ports.Object) bool { return o.Kind == kind && o.Name == name })
	if got != want {
		t.Fatalf("ListManaged(%s) contains %s %s = %v, want %v (%+v)", release, kind, name, got, want, objs)
	}
}

// FreePort returns a TCP port that was free a moment ago on 127.0.0.1.
func FreePort(t *testing.T) int { return freePort(t) }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
