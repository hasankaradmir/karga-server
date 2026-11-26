package docker

import (
	"net/netip"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// createOptions turns a request into Docker's create options without a daemon.
func TestCreateOptions(t *testing.T) {
	opts, err := createOptions(ports.WorkloadRequest{
		Name: "w", Image: "img", Command: []string{"run"}, Args: []string{"-v"}, RestartPolicy: "on-failure",
		Ports:    []ports.PortBinding{{ContainerPort: 8080, HostPort: 18080, HostIP: "127.0.0.1", Protocol: "tcp"}, {ContainerPort: 53, Protocol: "udp"}},
		Mounts:   []ports.Mount{{Type: ports.MountVolume, Source: "data", Target: "/data"}, {Type: ports.MountBind, Source: "/etc/x", Target: "/x", ReadOnly: true}},
		Networks: []string{"n1", "n2"}, Aliases: []string{"web"},
		MemoryBytes: 64 << 20, MilliCPU: 500, PidsLimit: 100, LogMaxSize: "10m", NoNewPrivileges: true, StopTimeout: 7 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, host := opts.Config, opts.HostConfig
	if opts.Name != "w" || cfg.Image != "img" || len(cfg.Cmd) != 2 || cfg.Cmd[1] != "-v" || *cfg.StopTimeout != 7 {
		t.Errorf("config: %+v", cfg)
	}
	if host.RestartPolicy.Name != container.RestartPolicyOnFailure {
		t.Errorf("restart: %v", host.RestartPolicy)
	}
	tcp, _ := network.PortFrom(8080, network.TCP)
	if b := host.PortBindings[tcp]; len(b) != 1 || b[0].HostIP != netip.MustParseAddr("127.0.0.1") || b[0].HostPort != "18080" {
		t.Errorf("binding: %v", host.PortBindings)
	}
	if udp, _ := network.PortFrom(53, network.UDP); len(host.PortBindings[udp]) != 0 || len(cfg.ExposedPorts) != 2 {
		t.Errorf("an unpublished port is exposed only: %v %v", host.PortBindings, cfg.ExposedPorts)
	}
	if len(host.Mounts) != 2 || host.Mounts[0].Type != mount.TypeVolume || host.Mounts[1].Type != mount.TypeBind || !host.Mounts[1].ReadOnly {
		t.Errorf("mounts: %+v", host.Mounts)
	}
	if host.Memory != 64<<20 || host.MemorySwap != 64<<20 || host.NanoCPUs != 500_000_000 || *host.PidsLimit != 100 {
		t.Errorf("limits: %+v", host.Resources)
	}
	if host.LogConfig.Type != "json-file" || host.LogConfig.Config["max-file"] != "1" || len(host.SecurityOpt) != 1 {
		t.Errorf("log %+v security %v", host.LogConfig, host.SecurityOpt)
	}
	if host.NetworkMode != "n1" || len(opts.NetworkingConfig.EndpointsConfig) != 1 || opts.NetworkingConfig.EndpointsConfig["n1"].Aliases[0] != "web" {
		t.Errorf("network: %v %+v", host.NetworkMode, opts.NetworkingConfig)
	}

	opts, err = createOptions(ports.WorkloadRequest{Name: "g", HostNetwork: true, RestartPolicy: "always"})
	if err != nil || opts.HostConfig.NetworkMode != network.NetworkHost || opts.NetworkingConfig != nil {
		t.Fatalf("host network: %v %v", opts.HostConfig.NetworkMode, err)
	}
}
