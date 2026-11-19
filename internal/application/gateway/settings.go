package gateway

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
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
		if l.Protocol != ProtocolTCP && l.Protocol != ProtocolHTTP && l.Protocol != ProtocolHTTPS {
			errs = append(errs, fmt.Errorf("listener %q: protocol %q must be tcp, http or https", l.Name, l.Protocol))
		}
		if _, err := l.limits(); err != nil {
			errs = append(errs, fmt.Errorf("listener %q: %w", l.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ports.ErrInvalid, errors.Join(errs...))
	}
	return nil
}

// defaultListeners are the listeners a new host starts with.
func defaultListeners() []Listener {
	return []Listener{{Name: "http", Port: 80, Protocol: ProtocolHTTP, ListenerOptions: ListenerOptions{RedirectHTTPS: true}},
		{Name: "https", Port: 443, Protocol: ProtocolHTTPS}}
}

// Listener is a port the gateway serves on every listen address. A tcp listener belongs to one
// service; http and https listeners are shared by host and path.
type Listener struct {
	Name            string `yaml:"name"`
	Port            int    `yaml:"port"`
	Protocol        string `yaml:"protocol"`
	ListenerOptions `yaml:",inline"`
}

// ListenerOptions are the administrator's limits of a listener, as written (api.ListenerOptions).
type ListenerOptions struct {
	Timeout       string `yaml:"timeout,omitempty"`
	IdleTimeout   string `yaml:"idleTimeout,omitempty"`
	MaxBody       string `yaml:"maxBody,omitempty"`
	RateLimit     string `yaml:"rateLimit,omitempty"`
	ProxyProtocol string `yaml:"proxyProtocol,omitempty"`
	RedirectHTTPS bool   `yaml:"redirectHTTPS,omitempty"`
}

// MaxBodyLimit bounds maxBody: Envoy holds a limited body in memory, within its 100MiB heap.
const MaxBodyLimit = 16 << 20

// limits parses the options. Those of http and https only are refused on tcp, and the other way.
func (l Listener) limits() (ports.ProxyHTTPLimits, error) {
	var lim ports.ProxyHTTPLimits
	var errs []error
	duration := func(field, v string) time.Duration {
		if v == "" {
			return 0
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a duration such as 30s", field, v))
		}
		return d
	}
	lim.Timeout = duration("timeout", l.Timeout)
	lim.IdleTimeout = duration("idleTimeout", l.IdleTimeout)
	if l.MaxBody != "" {
		n, err := resource.ParseMemory(l.MaxBody)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("maxBody: %w", err))
		case n < 1 || n > MaxBodyLimit:
			errs = append(errs, fmt.Errorf("maxBody: %s must be between 1 and 16Mi", l.MaxBody))
		default:
			lim.MaxBodyBytes = uint32(n)
		}
	}
	if l.RateLimit != "" {
		num, per, _ := strings.Cut(l.RateLimit, "/")
		n, err := strconv.ParseUint(num, 10, 32)
		lim.RatePer = map[string]time.Duration{"s": time.Second, "m": time.Minute}[per]
		if err != nil || n == 0 || lim.RatePer == 0 {
			errs = append(errs, fmt.Errorf("rateLimit: %q must be requests per second or minute, such as 100/s", l.RateLimit))
		}
		lim.RateRequests = uint32(n)
	}
	if l.ProxyProtocol != "" && l.ProxyProtocol != "v1" && l.ProxyProtocol != "v2" {
		errs = append(errs, fmt.Errorf("proxyProtocol: %q must be v1 or v2", l.ProxyProtocol))
	}
	var only []string
	if l.Protocol == ProtocolTCP {
		for f, v := range map[string]string{"timeout": l.Timeout, "maxBody": l.MaxBody, "rateLimit": l.RateLimit} {
			if v != "" {
				only = append(only, f)
			}
		}
	} else if l.ProxyProtocol != "" {
		only = append(only, "proxyProtocol")
	}
	if l.RedirectHTTPS && l.Protocol != ProtocolHTTP {
		only = append(only, "redirectHTTPS")
	}
	if len(only) > 0 {
		slices.Sort(only)
		errs = append(errs, fmt.Errorf("%s cannot be set on a %s listener", strings.Join(only, ", "), l.Protocol))
	}
	return lim, errors.Join(errs...)
}

// Settings are the host administrator's gateway, stored in $KARGA_HOME/gateway.yaml: where it
// listens, on which listeners, and whether it logs access. Packages cannot change them.
type Settings struct {
	// Listen are the host addresses every listener binds (default 127.0.0.1).
	Listen    []string   `yaml:"listen,omitempty"`
	Listeners []Listener `yaml:"listeners"`
	AccessLog bool       `yaml:"accessLog"`
}

// DefaultListen is where the gateway listens until the administrator sets addresses: nothing is
// reachable from other machines until the administrator says so.
const DefaultListen = "127.0.0.1"
