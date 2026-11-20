package deployment

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func httpSpec(port int, startPeriod string) *resource.WorkloadSpec {
	return &resource.WorkloadSpec{
		Ports: []resource.Port{{Name: "http", ContainerPort: port, Protocol: "tcp"}},
		HealthCheck: &resource.HealthCheck{Type: resource.HealthHTTP, Port: port, Path: "/health",
			Interval: "10ms", Timeout: "200ms", Retries: 2, StartPeriod: startPeriod},
	}
}

// probedWorkload is a running fake container whose network address is addr's host; it returns
// addr's port, which the health check uses.
func probedWorkload(t *testing.T, rt *fakeruntime.Runtime, name, addr string) int {
	t.Helper()
	host, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	if rt.IPs == nil {
		rt.IPs = map[string]map[string]string{}
	}
	rt.IPs[name] = map[string]string{"net": host}
	if err := rt.CreateWorkload(context.Background(), ports.WorkloadRequest{Name: name, Image: "img",
		Labels: map[string]string{ports.LabelManaged: "true"}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	return port
}

// crashLooping reports every workload restarting, one more restart on each look.
type crashLooping struct {
	*fakeruntime.Runtime
	restarts int
}

func (r *crashLooping) GetWorkload(ctx context.Context, name string) (ports.WorkloadStatus, error) {
	st, err := r.Runtime.GetWorkload(ctx, name)
	r.restarts++
	st.State, st.RestartCount, st.ExitCode = "restarting", r.restarts, 1
	return st, err
}

func TestWaitReadyStopsOnRestarts(t *testing.T) {
	rt := &crashLooping{Runtime: fakeruntime.New("img")}
	probedWorkload(t, rt.Runtime, "w", "127.0.0.1:1")
	start := time.Now()
	_, err := testEnv.waitReady(context.Background(), rt, []readyTarget{{"w", httpSpec(1, "")}}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "workload w keeps restarting (3 restarts, last exit code 1)") {
		t.Fatalf("want keeps restarting, got %v", err)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("waited %s for a crashing container", waited)
	}
}

func TestWaitReadyProbes(t *testing.T) {
	withProxySupport(t, true)
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	rt := fakeruntime.New("img")
	hp := probedWorkload(t, rt, "w", addr)
	ctx := context.Background()

	// Failing after the start period: unhealthy once retries are used up.
	if _, err := testEnv.waitReady(ctx, rt, []readyTarget{{"w", httpSpec(hp, "")}}, 5*time.Second); err == nil || !strings.Contains(err.Error(), "is unhealthy") {
		t.Fatalf("want unhealthy, got %v", err)
	}
	// Failures during the start period do not count; the server recovers within it.
	time.AfterFunc(150*time.Millisecond, func() { status.Store(http.StatusOK) })
	if notReady, err := testEnv.waitReady(ctx, rt, []readyTarget{{"w", httpSpec(hp, "1s")}}, 5*time.Second); err != nil || len(notReady) != 0 {
		t.Fatalf("want ready, got %v %v", notReady, err)
	}
	if got := testEnv.ProbeHealth(ctx, mustStatus(t, rt, "w"), httpSpec(hp, "")); got != ports.HealthHealthy {
		t.Fatalf("status health: %s", got)
	}

	// tcp: a closed port is unhealthy, an open one healthy.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tp := probedWorkload(t, rt, "t", l.Addr().String())
	tcp := &resource.WorkloadSpec{Ports: []resource.Port{{Name: "t", ContainerPort: tp, Protocol: "tcp"}},
		HealthCheck: &resource.HealthCheck{Type: resource.HealthTCP, Port: tp, Interval: "10ms", Retries: 1}}
	if _, err := testEnv.waitReady(ctx, rt, []readyTarget{{"t", tcp}}, 5*time.Second); err != nil {
		t.Fatalf("tcp open: %v", err)
	}
	_ = l.Close()
	if got := testEnv.ProbeHealth(ctx, mustStatus(t, rt, "t"), tcp); got != ports.HealthUnhealthy {
		t.Fatalf("tcp closed: %s", got)
	}
}

func mustStatus(t *testing.T, rt *fakeruntime.Runtime, name string) ports.WorkloadStatus {
	t.Helper()
	st, err := rt.GetWorkload(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestProbeNeedsLinux(t *testing.T) {
	withProxySupport(t, false)
	rs := manifest(6379)
	rs[0].Spec.(*resource.WorkloadSpec).HealthCheck = &resource.HealthCheck{Type: resource.HealthTCP, Port: 6379}
	if _, err := testEnv.Diff("cache", rs, nil, "", nil); err == nil || !strings.Contains(err.Error(), "needs Linux with a local, rootful Docker") {
		t.Fatalf("want unreachable error, got %v", err)
	}
}
