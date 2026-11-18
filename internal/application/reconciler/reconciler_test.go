package reconciler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/adapters/sqlite"
	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func TestEventTriggersRepair(t *testing.T) {
	Debounce = 50 * time.Millisecond
	deployment.Proxies = fakeruntime.Proxies{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	home := t.TempDir()
	store, err := sqlite.Open(ctx, filepath.Join(home, "karga.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rt := fakeruntime.New().WithGateway()
	rt.EventsC = make(chan ports.RuntimeEvent)
	svc := releases.New(store, func(context.Context) (ports.Runtime, error) { return rt, nil }, home, nil, nil)
	if _, err := svc.Install(ctx, releases.InstallRequest{CommonOptions: releases.CommonOptions{Timeout: api.Duration(time.Second)},
		Name: "cache", Package: "../../../tests/testdata/redis", Values: api.ValuesInput{Set: []string{"service.enabled=false"}}}, nil); err != nil {
		t.Fatal(err)
	}
	loop := New(svc, 0, nil) // events only
	go loop.Run(ctx)

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
