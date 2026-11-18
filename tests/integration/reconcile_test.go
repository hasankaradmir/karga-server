//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/reconciler"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// TestReconcilerRepairsFromDockerEvents: with the loop running (events only, no timer), a
// container removed, stopped or replaced behind karga's back is repaired from Docker's events.
func TestReconcilerRepairsFromDockerEvents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := releaseName("it-rec")
	s, rt := service(t, name)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"strategy=recreate"}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	loop := reconciler.New(reconciler.NewQueue(0), s.Reads, s.Lifecycle, rt,
		reconciler.Options{Only: func(rel string) bool { return rel == name }}) // other tests share the host
	go loop.Run(ctx)
	container := name + "-web"
	running := func(why string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if st, err := rt.GetWorkload(ctx, container); err == nil && st.State == ports.StateRunning {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not repaired", why)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	docker := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
	}

	docker("rm", "-f", container)
	running("removed container")
	docker("stop", container)
	running("stopped container")
	before := workload(t, rt, container).Labels[ports.LabelDigest]
	if before == "" {
		t.Fatal("no digest label on the container")
	}

	// The container runs before the reconciliation that started it is recorded as finished.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ops, err := s.Reads.Operations(ctx, name, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		var repairs []string
		for _, op := range ops {
			if op.Type == releases.ReconcileOperation {
				for _, st := range op.Steps {
					repairs = append(repairs, st.Action+" "+st.Name)
				}
			}
		}
		got := strings.Join(repairs, ",")
		if strings.Contains(got, "start web") && strings.Contains(got, "create web") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repairs recorded: %s", got)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if r := currentRelease(t, s, name); r.CurrentRevision != 1 {
		t.Fatalf("reconcile changed the revision: %+v", r)
	}
}
