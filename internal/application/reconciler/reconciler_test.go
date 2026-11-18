package reconciler

import (
	"context"
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
	svc, err := releases.New(releases.Deps{Store: store, Runtime: func(context.Context) (ports.Runtime, error) { return rt, nil },
		Host: &proxyops.Host{Proxies: fakeruntime.Proxies{}, Net: hostnet.Net{}}, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: releases.CommonOptions{Timeout: time.Second},
		Name: "cache", Package: "../../../tests/testdata/redis", Values: releases.Values{Sets: packages.Sets([]string{"service.enabled=false"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	loop := New(svc, 0, nil) // events only
	loop.Debounce = 50 * time.Millisecond
	stopped := make(chan struct{})
	go func() { loop.Run(ctx); close(stopped) }()
	// The repair records its operation after the container is back: let it finish before the
	// temporary state directory goes.
	defer func() { cancel(); <-stopped }()

	if err := rt.DeleteWorkload(ctx, "cache-redis"); err != nil {
		t.Fatal(err)
	}
	rt.EventsC <- ports.RuntimeEvent{Release: "cache", Container: "cache-redis", Action: "destroy"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err := rt.GetWorkload(ctx, "cache-redis"); err == nil && st.State == ports.StateRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the destroy event did not bring the container back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
