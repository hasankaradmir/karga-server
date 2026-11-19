package reconciler

import (
	"context"
	"fmt"
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

// The queue holds each release once and never drops one, however many wait.
func TestQueueKeepsEveryRelease(t *testing.T) {
	q := NewQueue(time.Millisecond)
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
