package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// allocatePort picks a free loopback port for an upstream. Docker binds it right after; a race
// with another process makes the container start fail, which is reported as usual.
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate an upstream port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ParseLabel reads the upstream ports of a proxy container.
func ParseLabel(labels map[string]string) []Port {
	var out []Port
	_ = json.Unmarshal([]byte(labels[LabelPorts]), &out) // karga writes it; a broken label means no ports
	return out
}

// labelSource records what a gateway container was created from; another KARGA_HOME or image
// means it must be recreated.
const labelSource = "io.karga.gateway-source"

func source(c ports.ProxyContainer, dir string) string {
	return dir + " " + c.Source
}

// containerName names the gateway made from source: ContainerName and 8 hex of the source, so a
// new one can start next to the one it replaces (both bind the listeners with SO_REUSEPORT).
func containerName(source string) string {
	sum := sha256.Sum256([]byte(source))
	return ContainerName + "-" + hex.EncodeToString(sum[:4])
}

// instance is the Envoy instance of gateway container name (its directory below Dir), "" for a
// container named ContainerName (before D38), which uses Dir itself.
func instance(name string) string {
	inst, _ := strings.CutPrefix(name, ContainerName+"-")
	if inst == name {
		return ""
	}
	return inst
}

// adminDir is the directory with the admin socket of gateway container name.
func adminDir(home, name string) string { return filepath.Join(Dir(home), instance(name)) }

func request(h ports.ProxyControl, dir string) ports.WorkloadRequest {
	c := h.Container()
	name := containerName(source(c, dir))
	return ports.WorkloadRequest{
		Name:   name,
		Labels: map[string]string{ports.LabelManaged: "true", LabelComponent: "gateway", labelSource: source(c, dir)},
		Image:  c.Image,
		// Root only to bind low ports in the host namespace (Docker grants no ambient capabilities
		// to other users); every capability but those two is dropped.
		Command:         h.Command(instance(name)),
		User:            "0:0",
		Env:             map[string]string{"ENVOY_UID": "0"},
		HostNetwork:     true,
		CapDrop:         []string{"ALL"},
		CapAdd:          []string{"NET_BIND_SERVICE", "DAC_OVERRIDE"},
		NoNewPrivileges: true,
		RestartPolicy:   "always",
		Mounts:          []ports.Mount{{Type: ports.MountBind, Source: dir, Target: c.MountPath}},
		MemoryBytes:     c.MemoryBytes,
		MilliCPU:        c.MilliCPU,
		PidsLimit:       c.Pids,
		LogMaxSize:      resource.DefaultLogMaxSize,
		LogMaxFiles:     resource.DefaultLogMaxFiles,
	}
}

// Publish publishes service ports of a release proxy request on fresh loopback upstream ports and
// records them in its labels.
func Publish(wr *ports.WorkloadRequest, published map[string][]int) error {
	var label []Port
	for _, svc := range slices.Sorted(maps.Keys(published)) {
		for _, port := range published[svc] {
			up, err := allocatePort()
			if err != nil {
				return err
			}
			wr.Ports = append(wr.Ports, ports.PortBinding{ContainerPort: port, HostPort: up, HostIP: "127.0.0.1", Protocol: "tcp"})
			label = append(label, Port{Service: svc, Port: port, Upstream: up})
		}
	}
	if len(label) == 0 {
		return nil
	}
	js, err := json.Marshal(label)
	if err != nil {
		return err
	}
	wr.Labels = maps.Clone(wr.Labels)
	wr.Labels[LabelPorts] = string(js)
	return nil
}
