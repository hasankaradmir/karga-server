package gateway

import (
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/resource"
)

// listenerOf returns the listener of s called name.
func listenerOf(s Settings, name string) (Listener, bool) {
	i := slices.IndexFunc(s.Listeners, func(l Listener) bool { return l.Name == name })
	if i < 0 {
		return Listener{}, false
	}
	return s.Listeners[i], true
}

// validateSettings checks the administrator's settings.
func validateSettings(s Settings) error {
	var errs []error
	for _, ip := range s.Listen {
		if net.ParseIP(ip) == nil {
			errs = append(errs, fmt.Errorf("listen: %q is not an IP address", ip))
		}
	}
	names, portsUsed := map[string]bool{}, map[int]string{}
	for _, l := range s.Listeners {
		switch {
		case !resource.IsDNSLabel(l.Name):
			errs = append(errs, fmt.Errorf("listener %q: the name must be a DNS label", l.Name))
		case names[l.Name]:
			errs = append(errs, fmt.Errorf("listener %q is defined twice", l.Name))
		}
		names[l.Name] = true
		if l.Port < 1 || l.Port > 65535 {
			errs = append(errs, fmt.Errorf("listener %q: port %d out of range 1-65535", l.Name, l.Port))
		} else if other, dup := portsUsed[l.Port]; dup {
			errs = append(errs, fmt.Errorf("listener %q: port %d is used by listener %q", l.Name, l.Port, other))
		}
		portsUsed[l.Port] = l.Name
		if l.Protocol != api.ProtocolTCP && l.Protocol != api.ProtocolHTTP && l.Protocol != api.ProtocolHTTPS {
			errs = append(errs, fmt.Errorf("listener %q: protocol %q must be tcp, http or https", l.Name, l.Protocol))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", api.ErrInvalid, errors.Join(errs...))
	}
	return nil
}

// DefaultListeners are the listeners a new host starts with.
func DefaultListeners() []Listener {
	return []Listener{{Name: "web", Port: 80, Protocol: api.ProtocolHTTP}, {Name: "websecure", Port: 443, Protocol: api.ProtocolHTTPS}}
}
