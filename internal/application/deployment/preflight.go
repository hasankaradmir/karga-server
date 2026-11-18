package deployment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Preflight finds problems before anything is created: runtime support, missing capabilities and
// exposures the gateway cannot serve (unknown listeners, or ones other releases or host processes
// hold). What the same release already holds is allowed (an upgrade keeps it).
// allowedCaps are the capabilities, besides resource.DefaultCaps, the host administrator allows
// packages to add.
func (e *Env) Preflight(ctx context.Context, rt ports.Runtime, info ports.RuntimeInfo, home, release string, supported []string, rs []resource.Resource, allowedCaps []string) error {
	var errs []error
	if !slices.Contains(supported, info.Name) {
		errs = append(errs, fmt.Errorf("package does not support runtime %q (runtime.supported: %v)", info.Name, supported))
	}
	need := func(r resource.Resource, capability, what string) {
		if !info.Has(capability) {
			errs = append(errs, fmt.Errorf("%s %q: %s is not supported by %s %s", r.Kind, r.Metadata.Name, what, info.Name, info.Version))
		}
	}
	for _, r := range rs {
		switch s := r.Spec.(type) {
		case *resource.NetworkSpec:
			need(r, ports.CapNetworkDriver(s.Driver), "network driver "+s.Driver)
		case *resource.VolumeSpec:
			need(r, ports.CapVolumeDriver(s.Driver), "volume driver "+s.Driver)
		case *resource.WorkloadSpec:
			if s.Resources != nil && s.Resources.Memory != "" {
				need(r, ports.CapResourcesMemory, "a memory limit")
			}
			if s.Resources != nil && s.Resources.CPU != "" {
				need(r, ports.CapResourcesCPU, "a cpu limit")
			}
			if s.HealthCheck != nil && s.HealthCheck.Type == resource.HealthExec {
				need(r, ports.CapHealthcheckExec, "an exec healthcheck")
			}
			if s.Security != nil {
				for _, c := range s.Security.CapAdd {
					if !capAllowed(c, allowedCaps) {
						errs = append(errs, fmt.Errorf("%s %q: capability %s is not allowed: it gives the container power over the host. "+
							"The host administrator can allow it with karga-server --allow-capabilities %s", r.Kind, r.Metadata.Name, c, capName(c)))
					}
				}
			}
		}
	}

	if e.Supported() {
		errs = append(errs, gateway.Check(ctx, rt, e.Host, home, release, rs)...)
	}
	return errors.Join(errs...)
}

// capName is a capability name without the optional CAP_ prefix, in upper case.
func capName(c string) string { return strings.TrimPrefix(strings.ToUpper(c), "CAP_") }

func capAllowed(c string, allowed []string) bool {
	c = capName(c)
	if slices.Contains(resource.DefaultCaps(), c) {
		return true
	}
	for _, a := range allowed {
		if a = capName(a); a == c || a == "ALL" {
			return true
		}
	}
	return false
}
