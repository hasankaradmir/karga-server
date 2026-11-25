package reconciler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func TestEventTriggersRepair(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	home := t.TempDir()
	store, err := filestore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rt := fakeruntime.New().WithGateway()
	rt.EventsC = make(chan ports.RuntimeEvent)
	q := NewQueue(50 * time.Millisecond)
	host := &proxyops.Host{Proxies: fakeruntime.Proxies{}, Net: hostnet.Net{}}
	info, err := rt.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host.UseRuntime(info)
	svc, err := releases.New(releases.Deps{Store: store, Runtime: rt, Host: host, Home: home, Notify: q.Notify})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: releases.CommonOptions{Timeout: time.Second},
		Name: "cache", Package: "../../../tests/testdata/redis", Values: releases.Values{Sets: packages.Sets([]string{"service.enabled=false"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	loop := New(q, svc.Reads, svc.Lifecycle, rt, Options{}) // events only
	stopped := make(chan struct{})
	go func() { loop.Run(ctx); close(stopped) }()
	// The repair records its operation after the container is back: let it finish before the
	// temporary state directory goes.
	defer func() { cancel(); <-stopped }()

	if err := rt.DeleteWorkload(ctx, rt.Name("cache-redis")); err != nil {
		t.Fatal(err)
	}
	rt.EventsC <- ports.RuntimeEvent{Release: "cache", Container: "cache-redis", Action: "destroy"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err := rt.GetWorkload(ctx, rt.Name("cache-redis")); err == nil && st.State == ports.StateRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the destroy event did not bring the container back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A Notify that held the lock while the release's timer fired resets a timer whose run is waiting
// for the lock: the timer fires again, and that second run must not hand the release out twice.
func TestQueueResetAfterFiringDeliversOnce(t *testing.T) {
	q := NewQueue(time.Millisecond)
	q.Notify("a")
	q.mu.Lock()
	time.Sleep(20 * time.Millisecond)           // the timer fires; its run waits for the lock
	q.pending["a"].Reset(50 * time.Millisecond) // what Notify does when it finds "a" pending
	q.mu.Unlock()
	<-q.wake
	if got := q.take(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("first: %v", got)
	}
	time.Sleep(100 * time.Millisecond) // past the reset
	if got := q.take(); len(got) != 0 {
		t.Fatalf("handed out again: %v", got)
	}
}

// The queue holds each release once and never drops one, however many wait. The debounce outlasts
// the notifications: a second Notify after the release came out queues it again, rightly.
func TestQueueKeepsEveryRelease(t *testing.T) {
	q := NewQueue(100 * time.Millisecond)
	for i := range 300 {
		q.Notify(fmt.Sprintf("r%d", i))
		q.Notify(fmt.Sprintf("r%d", i)) // twice: once in the queue
	}
	got := map[string]int{}
	deadline := time.After(5 * time.Second)
	for len(got) < 300 {
		select {
		case <-q.wake:
			for _, n := range q.take() {
				got[n]++
			}
		case <-deadline:
			t.Fatalf("%d of 300 releases came out of the queue", len(got))
		}
	}
	for n, c := range got {
		if c != 1 {
			t.Fatalf("%s came out %d times", n, c)
		}
	}
}

// brokenStream is an event stream that breaks at once, saying why.
type brokenStream struct{}

func (brokenStream) Events(context.Context) (<-chan ports.RuntimeEvent, error) {
	c := make(chan ports.RuntimeEvent, 1)
	c <- ports.RuntimeEvent{Err: errors.New("unexpected EOF")}
	close(c)
	return c, nil
}

// A broken event stream is logged with its reason; the loop reconnects.
func TestBrokenEventStreamIsLogged(t *testing.T) {
	var buf bytes.Buffer
	l := New(NewQueue(time.Hour), nil, nil, brokenStream{}, Options{Log: slog.New(slog.NewTextHandler(&buf, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.watch(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	if out := buf.String(); !strings.Contains(out, `msg="reconcile: runtime events stopped" error="unexpected EOF"`) {
		t.Fatalf("log:\n%s", out)
	}
}

// failing is a Repairer whose repairs fail with err until it is nil.
type failing struct{ err error }

func (f *failing) Repair(context.Context, string, bool) (*releases.ReconcileResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &releases.ReconcileResult{}, nil
}

func (f *failing) NextRepair(string) time.Time { return time.Time{} }

// A failure that repeats every pass (Docker down) is logged once, and its end once.
func TestRepeatedFailureIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	f := &failing{err: errors.New("Cannot connect to the Docker daemon")}
	l := New(NewQueue(time.Hour), nil, f, brokenStream{}, Options{Log: slog.New(slog.NewTextHandler(&buf, nil))})
	ctx := context.Background()
	for range 3 {
		l.one(ctx, "cache")
	}
	f.err = nil
	l.one(ctx, "cache")
	l.one(ctx, "cache")
	out := buf.String()
	if strings.Count(out, "Cannot connect") != 1 || strings.Count(out, `msg="reconcile works again"`) != 1 {
		t.Fatalf("log:\n%s", out)
	}
}
