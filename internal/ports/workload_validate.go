package ports

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// RestartPolicies are the restart policies a WorkloadRequest may ask for; "" is "never".
var RestartPolicies = []string{"", "always", "on-failure", "unless-stopped", "never"}

// Validate checks r as every Runtime does before it creates anything, so that a fake runtime
// refuses what Docker refuses: the error wraps ErrInvalid.
func (r WorkloadRequest) Validate() error {
	var errs []error
	if !slices.Contains(RestartPolicies, r.RestartPolicy) {
		errs = append(errs, fmt.Errorf("unknown restart policy %q", r.RestartPolicy))
	}
	if r.HostNetwork && (len(r.Networks) > 0 || len(r.Ports) > 0) {
		errs = append(errs, errors.New("a workload on the host network has no networks or ports"))
	}
	for _, p := range r.Ports {
		if p.ContainerPort < 1 || p.ContainerPort > 65535 {
			errs = append(errs, fmt.Errorf("invalid port %d", p.ContainerPort))
		}
		if !slices.Contains([]string{"tcp", "udp", "sctp"}, strings.ToLower(p.Protocol)) {
			errs = append(errs, fmt.Errorf("port %d: invalid protocol %q", p.ContainerPort, p.Protocol))
		}
		if p.HostPort == 0 {
			continue
		}
		if p.HostPort < 1 || p.HostPort > 65535 {
			errs = append(errs, fmt.Errorf("invalid host port %d", p.HostPort))
		}
		if _, err := netip.ParseAddr(p.HostIP); err != nil {
			errs = append(errs, fmt.Errorf("host IP %q: %w", p.HostIP, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("workload %s: %w: %w", r.Name, ErrInvalid, errors.Join(errs...))
}
