package fakeruntime

import (
	"testing"

	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

func TestContract(t *testing.T) {
	rt := New("redis:7.4")
	rt.ImageVolumes = map[string][]string{"redis:7.4": {"/data"}}
	runtimetest.Run(t, rt, runtimetest.Config{
		Image:          "busybox:1.37",
		SleepCommand:   []string{"sleep", "300"},
		HealthyCommand: []string{"true"},
		VolumeImage:    "redis:7.4",
		VolumePath:     "/data",
	})
}
