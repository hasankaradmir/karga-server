package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

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

// instance is the Envoy instance of gateway container name (its directory below Dir).
func instance(name string) string { return strings.TrimPrefix(name, ContainerName+"-") }

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
