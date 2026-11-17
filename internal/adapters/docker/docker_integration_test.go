//go:build integration

package docker

import (
	"context"
	"testing"

	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

func TestContract(t *testing.T) {
	rt, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	runtimetest.Run(t, rt, runtimetest.Config{
		Image:          "busybox:1.37",
		SleepCommand:   []string{"sleep", "300"},
		HealthyCommand: []string{"true"},
		ReportsIPs:     true,
		ExecCommand:    []string{"sh", "-c", "cat; echo to stderr >&2; exit 3"},
		VolumeImage:    "redis:7.4-alpine",
		VolumePath:     "/data",
	})
}
