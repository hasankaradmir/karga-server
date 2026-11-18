package deployment

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

func labels(release string, revision int, kind, name string) map[string]string {
	return map[string]string{
		ports.LabelManaged:  "true",
		ports.LabelRelease:  release,
		ports.LabelKind:     kind,
		ports.LabelName:     name,
		ports.LabelRevision: strconv.Itoa(revision),
	}
}

// workloadRequest builds the container of a workload. It publishes no port and gets no alias of its
// name: behind the release proxy (a Service targets it) it answers to its revision's backend alias,
// meant for the proxy.
func workloadRequest(release, name string, s *resource.WorkloadSpec, lbl map[string]string, paths map[Key]string, behind bool) (ports.WorkloadRequest, error) {
	wr := ports.WorkloadRequest{
		Name:          proxyops.RuntimeName(release, name),
		Labels:        lbl,
		Image:         s.Image,
		Command:       s.Command,
		Args:          s.Args,
		Env:           s.Environment,
		RestartPolicy: s.RestartPolicy,
		StopTimeout:   s.StopGrace(),
	}
	if len(s.EnvironmentFrom) > 0 {
		wr.Env = maps.Clone(s.Environment)
		if wr.Env == nil {
			wr.Env = map[string]string{}
		}
		// The values come from the Secret's materialized files: the stored manifest holds only the
		// reference, and Materialize has registered them with the log redactor.
		for _, e := range s.EnvironmentFrom {
			dir, ok := paths[Key{resource.KindSecret, e.Secret}]
			if !ok {
				return wr, fmt.Errorf("environmentFrom %s: Secret %q was not materialized", e.Name, e.Secret)
			}
			v, err := os.ReadFile(filepath.Join(dir, e.Key))
			if err != nil {
				return wr, fmt.Errorf("environmentFrom %s: %w", e.Name, err)
			}
			wr.Env[e.Name] = string(v)
		}
	}
	for _, n := range s.Networks {
		wr.Networks = append(wr.Networks, proxyops.RuntimeName(release, n))
	}
	for _, m := range s.Mounts {
		kind, target := m.Target()
		if kind == resource.KindVolume {
			wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountVolume, Source: proxyops.RuntimeName(release, target), Target: m.MountPath, ReadOnly: m.ReadOnly})
			continue
		}
		dir, ok := paths[Key{kind, target}]
		if !ok {
			return wr, fmt.Errorf("mount %s: %s %q was not materialized", m.MountPath, kind, target)
		}
		wr.Mounts = append(wr.Mounts, ports.Mount{Type: ports.MountBind, Source: dir, Target: m.MountPath, ReadOnly: true})
	}
	if behind {
		rev, _ := strconv.Atoi(lbl[ports.LabelRevision])
		wr.Aliases = []string{BackendAlias(name, rev)}
	}
	if hc := s.HealthCheck; hc != nil && hc.Type == resource.HealthExec {
		// Durations were validated when the resource was decoded.
		interval, _ := time.ParseDuration(hc.Interval)
		timeout, _ := time.ParseDuration(hc.Timeout)
		start, _ := time.ParseDuration(hc.StartPeriod)
		wr.HealthCheck = &ports.HealthCheck{Command: hc.Command, Interval: interval, Timeout: timeout, Retries: hc.Retries, StartPeriod: start}
	}
	wr.NoNewPrivileges = true
	if sec := s.Security; sec != nil {
		wr.NoNewPrivileges = sec.NoNewPrivileges == nil || *sec.NoNewPrivileges
		wr.CapDrop, wr.CapAdd, wr.User = sec.CapDrop, sec.CapAdd, sec.User
		if sec.ReadOnlyRootFilesystem {
			wr.ReadOnlyRootfs = true
			wr.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"}
		}
	}
	wr.PidsLimit = resource.DefaultPids
	wr.LogMaxSize, wr.LogMaxFiles = resource.DefaultLogMaxSize, resource.DefaultLogMaxFiles
	if l := s.Logging; l != nil && l.MaxSize != "" {
		wr.LogMaxSize, wr.LogMaxFiles = l.MaxSize, l.MaxFiles
	}
	if r := s.Resources; r != nil && r.Pids != nil {
		wr.PidsLimit = *r.Pids
	}
	if r := s.Resources; r != nil {
		if r.Memory != "" {
			wr.MemoryBytes, _ = resource.ParseMemory(string(r.Memory))
		}
		if r.CPU != "" {
			wr.MilliCPU, _ = resource.ParseCPU(string(r.CPU))
		}

	}
	return wr, nil
}
