package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Warning is a problem Check found that does not stop a deployment.
type Warning string

func (w Warning) Error() string { return string(w) }

// IsWarning reports whether err is a Warning.
func IsWarning(err error) bool {
	var w Warning
	return errors.As(err, &w)
}

// Check refuses exposures of release rel that the gateway cannot serve: an unknown listener, a
// listener of another protocol, or a tcp listener, host and path or certificate another release
// already holds. It binds a listener nobody uses yet once, to see that no other process holds it.
func Check(ctx context.Context, rt Runtime, h *proxyops.Host, home, rel string, rs []resource.Resource) []error {
	settings, err := ReadSettings(home)
	if err != nil {
		return []error{err}
	}
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return []error{err}
	}
	type owner struct{ release, service string }
	tcp := map[string]owner{}    // listener -> owner
	routes := map[string]owner{} // listener host+path -> owner
	certs := map[string]string{} // https host -> release/secret
	inUse := map[string]bool{}   // listeners the gateway serves already
	for _, t := range targets {
		for _, r := range t.Routes {
			inUse[r.Listener] = true
			if t.Release == rel {
				continue
			}
			o := owner{t.Release, r.Service}
			if r.Host == "" {
				tcp[r.Listener] = o
			} else {
				routes[r.Listener+" "+r.Host+r.Path] = o
			}
			if r.TLSSecret != "" {
				certs[r.Host] = t.Release + "/" + r.TLSSecret
			}
		}
	}
	var errs []error
	tested := map[string]bool{}
	for _, r := range rs {
		s, ok := r.Spec.(*resource.ServiceSpec)
		if !ok {
			continue
		}
		for _, e := range s.Expose {
			where := fmt.Sprintf("%s %q: expose %s on listener %s", resource.KindService, r.Metadata.Name, e.Port, e.Listener)
			l, ok := listenerOf(settings, e.Listener)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: no such gateway listener; the host's administrator adds it with 'karga gateway listener add %s --port <port> --protocol tcp|http|https'", where, e.Listener))
				continue
			}
			switch {
			case l.Protocol == ProtocolTCP && e.Host != "":
				errs = append(errs, fmt.Errorf("%s: %s is a tcp listener and takes no host, path or tls", where, l.Name))
				continue
			case l.Protocol != ProtocolTCP && e.Host == "":
				errs = append(errs, fmt.Errorf("%s: %s is an %s listener and needs a host", where, l.Name, l.Protocol))
				continue
			case l.Protocol == ProtocolHTTPS && e.TLS == nil:
				errs = append(errs, fmt.Errorf("%s: %s is an https listener and needs tls.secret", where, l.Name))
				continue
			case l.Protocol == ProtocolHTTP && e.TLS != nil:
				errs = append(errs, fmt.Errorf("%s: %s is a plain http listener; use an https listener for tls", where, l.Name))
				continue
			}
			if o, taken := tcp[l.Name]; taken {
				errs = append(errs, fmt.Errorf("%s: the listener is used by %s/%s: %w", where, o.release, o.service, ports.ErrPortInUse))
			}
			if o, taken := routes[l.Name+" "+e.Host+e.Path]; taken && e.Host != "" {
				errs = append(errs, fmt.Errorf("%s: %s%s is already routed to %s/%s: %w", where, e.Host, e.Path, o.release, o.service, ports.ErrPortInUse))
			}
			if e.TLS != nil && certs[e.Host] != "" && certs[e.Host] != rel+"/"+e.TLS.Secret {
				errs = append(errs, fmt.Errorf("%s: %s already serves HTTPS for %s with another certificate: %w", where, strings.SplitN(certs[e.Host], "/", 2)[0], e.Host, ports.ErrPortInUse))
			}
			if !inUse[l.Name] && !tested[l.Name] {
				tested[l.Name] = true
				for _, ip := range settings.Listen {
					switch taken, err := h.Net.PortTaken("tcp", ip, l.Port); {
					case taken:
						errs = append(errs, fmt.Errorf("%s: %s:%d is taken by another process on this host: %w", where, ip, l.Port, ports.ErrPortInUse))
					case err != nil:
						errs = append(errs, Warning(fmt.Sprintf("%s:%d could not be checked (%v); if another process holds it, the gateway reports it when it starts",
							ip, l.Port, errors.Unwrap(err))))
					}
				}
			}
		}
	}
	return errs
}
