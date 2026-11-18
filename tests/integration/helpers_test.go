//go:build integration

package integration

// Shared helpers of the integration tests: services against real Docker, and clients that measure
// what the host and other workloads see while karga changes things.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/envoy"

	"github.com/hasankaradmir/karga-server/internal/adapters/docker"
	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

// webPkg is a small HTTP server package; see testdata/web/values.yaml for its knobs.
const webPkg = "testdata/web"

// sharedHome is the KARGA_HOME of every test. The host gateway is one container configured from
// one home, so tests running in parallel must share it. Tests that need the host to themselves
// (the gateway going away, port 80, a Docker restart) do not call t.Parallel(): Go runs them
// before the parallel ones start.
var sharedHome string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "karga-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sharedHome = dir
	if runtime.GOOS != "linux" {
		// Services need karga's gateway and release proxy: Linux with a local Docker.
		fmt.Fprintln(os.Stderr, "integration tests run on Linux with a local Docker: use make test-linux")
		os.Exit(1)
	}
	// The host has one gateway, and karga-server leaves another home's alone: one an earlier run
	// left behind would stop this run's exposures.
	removeGateway()
	code := m.Run()
	removeGateway()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// removeGateway removes the host gateway container, whichever home it serves.
func removeGateway() { _ = exec.Command("docker", "rm", "-f", gateway.ContainerName).Run() }

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the proxy only runs on Linux (ADR-0009)")
	}
}

// hostPinger requests http://127.0.0.1:port/ (with an optional Host header) every 50ms from the
// host until stopped.
type hostPinger struct {
	ok, failed atomic.Int64
	mu         sync.Mutex
	bodies     map[string]int
	stop       chan struct{}
	done       sync.WaitGroup
}

func startHostPinger(port int) *hostPinger { return startHostPingerFor(port, "") }

func startHostPingerFor(port int, host string) *hostPinger {
	p := &hostPinger{bodies: map[string]int{}, stop: make(chan struct{})}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		for {
			select {
			case <-p.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			if host != "" {
				req.Host = host
			}
			resp, err := client.Do(req)
			if err != nil {
				p.failed.Add(1)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				p.failed.Add(1)
				continue
			}
			p.ok.Add(1)
			p.mu.Lock()
			p.bodies[strings.TrimSpace(string(b))]++
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *hostPinger) halt() { close(p.stop); p.done.Wait() }

// seen returns a copy of the response bodies counted so far.
func (p *hostPinger) seen() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.bodies)
}

func (p *hostPinger) reset() { p.ok.Store(0); p.failed.Store(0) }

// hostGet does one request to http://127.0.0.1:port/ with a Host header and returns status and body.
func hostGet(t *testing.T, port int, host string) (int, string) {
	t.Helper()
	code, body, err := tryGet(port, host)
	if err != nil {
		t.Fatalf("GET %s on %d: %v", host, port, err)
	}
	return code, body
}

// tryGet is hostGet returning the error.
func tryGet(port int, host string) (int, string, error) {
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/", nil)
	req.Host = host
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func service(t *testing.T, name string) (*releases.Service, ports.Runtime) {
	t.Helper()
	return serviceIn(t, sharedHome, name)
}

// serviceIn is service with a given KARGA_HOME, so several services can share one.
func serviceIn(t *testing.T, home, name string) (*releases.Service, ports.Runtime) {
	t.Helper()
	rt := newRuntime(t)
	store, err := filestore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hostEnv(home).Delete(context.Background(), rt, name, true); err != nil {
			t.Errorf("cleanup %s: %v", name, err)
		}
		_ = store.Close()
	})
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	testHost.UseRuntime(info)
	svc, err := releases.New(releases.Deps{Store: store, Runtime: rt, Host: testHost, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return svc, rt
}

// listener adds a gateway listener of protocol on a free port to the shared KARGA_HOME, as the
// host's administrator would, and returns its name and port. Listeners outlive the test.
func listener(t *testing.T, protocol string) (string, int) {
	t.Helper()
	port := runtimetest.FreePort(t)
	name := fmt.Sprintf("l%d", port)
	unlock, err := gateway.Lock(context.Background(), sharedHome)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	settings, err := gateway.ReadSettings(sharedHome)
	if err == nil {
		settings.Listeners = append(settings.Listeners, gateway.Listener{Name: name, Port: port, Protocol: protocol})
		err = gateway.WriteSettings(sharedHome, settings)
	}
	if err != nil {
		t.Fatal(err)
	}
	return name, port
}

func opts() releases.CommonOptions {
	return releases.CommonOptions{Timeout: 2 * time.Minute}
}

func workload(t *testing.T, rt ports.Runtime, name string) ports.WorkloadStatus {
	t.Helper()
	st, err := rt.GetWorkload(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func currentRelease(t *testing.T, s *releases.Service, name string) release.Release {
	t.Helper()
	rels, err := s.Reads.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rels {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("release %s not listed", name)
	return release.Release{}
}

// readFile returns a values file's content, as the CLI sends it.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newRuntime(t *testing.T) *docker.Runtime {
	t.Helper()
	rt, err := docker.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func releaseName(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// netPinger measures what other workloads see: a container on the release network requests each
// URL every 100ms. It has to be a container there because Docker resolves aliases such as "web"
// only on the network, never from the host. It is test-only; karga starts no such container.
type netPinger struct {
	t         *testing.T
	container string
	urls      []string
}

// pingResult counts a pinger's requests to one URL.
type pingResult struct {
	OK, Failed int
	Bodies     map[string]int
}

func (r pingResult) String() string {
	return fmt.Sprintf("%d ok, %d failed, bodies %v", r.OK, r.Failed, r.Bodies)
}

func startNetPinger(t *testing.T, rt ports.Runtime, rel string, urls ...string) *netPinger {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("%s-pinger-%d", rel, time.Now().UnixNano()%100000)
	script := `while :; do i=0; for u in $URLS; do b=$(wget -q -T 2 -O - "$u" 2>/dev/null) && echo "$i OK $b" || echo "$i FAIL"; i=$((i+1)); done; sleep 0.1; done`
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{
		Name: name, Image: "busybox:1.37", RestartPolicy: "never",
		Labels:   map[string]string{ports.LabelManaged: "true", ports.LabelRelease: rel},
		Networks: []string{rel + "-web-net"},
		Env:      map[string]string{"URLS": strings.Join(urls, " ")},
		Command:  []string{"sh", "-c", script},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, name); err != nil {
		t.Fatal(err)
	}
	return &netPinger{t: t, container: name, urls: urls}
}

// results reads the pinger's log: one result per URL, in the order given.
func (p *netPinger) results() []pingResult {
	p.t.Helper()
	out, err := exec.Command("docker", "logs", p.container).CombinedOutput()
	if err != nil {
		p.t.Fatalf("docker logs %s: %v\n%s", p.container, err, out)
	}
	res := make([]pingResult, len(p.urls))
	for i := range res {
		res[i].Bodies = map[string]int{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(line, " ", 3)
		i, err := strconv.Atoi(f[0])
		if err != nil || i < 0 || i >= len(res) || len(f) < 2 {
			continue
		}
		if f[1] == "OK" {
			res[i].OK++
			if len(f) == 3 {
				res[i].Bodies[f[2]]++
			}
		} else {
			res[i].Failed++
		}
	}
	return res
}

// result is the result of a pinger with one URL.
func (p *netPinger) result() pingResult { return p.results()[0] }

// testHost is the host every test deploys on: Envoy, the host's network.
var testHost = &proxyops.Host{Proxies: envoy.Proxies{}, Net: hostnet.Net{}}

// hostEnv deploys on testHost with home as the state home.
func hostEnv(home string) *deployment.Env {
	return deployment.NewEnv(testHost, deployment.EnvConfig{Home: home})
}
