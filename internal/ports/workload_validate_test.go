package ports

import (
	"errors"
	"testing"
)

func TestWorkloadRequestValidate(t *testing.T) {
	ok := func() WorkloadRequest {
		return WorkloadRequest{Name: "w", Image: "i", RestartPolicy: "always",
			Ports: []PortBinding{{ContainerPort: 8080, HostPort: 18080, HostIP: "127.0.0.1", Protocol: "tcp"}, {ContainerPort: 53, Protocol: "udp"}}}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("a valid request: %v", err)
	}
	host := WorkloadRequest{Name: "g", HostNetwork: true, RestartPolicy: "always"}
	if err := host.Validate(); err != nil {
		t.Fatalf("a host-network request: %v", err)
	}
	for name, change := range map[string]func(*WorkloadRequest){
		"unknown restart policy":    func(r *WorkloadRequest) { r.RestartPolicy = "sometimes" },
		"container port 0":          func(r *WorkloadRequest) { r.Ports[0].ContainerPort = 0 },
		"container port too large":  func(r *WorkloadRequest) { r.Ports[0].ContainerPort = 65536 },
		"no protocol":               func(r *WorkloadRequest) { r.Ports[1].Protocol = "" },
		"unknown protocol":          func(r *WorkloadRequest) { r.Ports[1].Protocol = "icmp" },
		"host port too large":       func(r *WorkloadRequest) { r.Ports[0].HostPort = 70000 },
		"published, no host IP":     func(r *WorkloadRequest) { r.Ports[0].HostIP = "" },
		"published, bad host IP":    func(r *WorkloadRequest) { r.Ports[0].HostIP = "localhost" },
		"host network and networks": func(r *WorkloadRequest) { r.HostNetwork, r.Ports, r.Networks = true, nil, []string{"n"} },
		"host network and ports":    func(r *WorkloadRequest) { r.HostNetwork = true },
	} {
		r := ok()
		change(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
