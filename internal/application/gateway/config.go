package gateway

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Cluster names the cluster of one service port. Release and service names are DNS labels, so
// "_" cannot occur in them.
func Cluster(rel, service string, port int) string {
	return fmt.Sprintf("%s_%s_p%d", rel, service, port)
}

// certName is the file name (without extension) of a TLS host's certificate in the gateway
// directory. It includes a digest of the directory the certificate comes from: Envoy reads
// certificate files only when the listener changes, so a new certificate needs a new name.
func certName(rel, listener, host, src string) string {
	sum := sha256.Sum256([]byte(src))
	return fmt.Sprintf("%s_%s_%s_%x", rel, listener, host, sum[:4])
}

// use is what the targets attach to one listener.
type use struct {
	tcp    string // cluster
	routes []ports.ProxyRoute
	certs  map[string]ports.ProxyCert
	seen   map[string]bool
}

// buildConfig builds the gateway's listeners and clusters: every listener in use, on every listen
// address. A listener nobody uses is not opened. Conflicts are refused by Check; if one slips
// through anyway (two releases applied at once) the first release in name order wins. A host
// served on an https listener also gets a redirect on the http listeners of port 80.
func buildConfig(settings Settings, targets []Target, mountPath string) ports.ProxyConfig {
	var cfg ports.ProxyConfig
	uses := map[string]*use{}
	for _, t := range targets {
		for _, p := range t.Ports {
			cfg.Clusters = append(cfg.Clusters, ports.ProxyCluster{Name: Cluster(t.Release, p.Service, p.Port), Host: t.IP, Port: p.Port,
				HealthPath: healthPath(t.Routes, p)})
		}
		for _, r := range t.Routes {
			l, ok := listenerOf(settings, r.Listener)
			if _, published := t.upstream(r.Service, r.Port); !ok || !published {
				continue
			}
			u := uses[l.Name]
			if u == nil {
				u = &use{certs: map[string]ports.ProxyCert{}, seen: map[string]bool{}}
				uses[l.Name] = u
			}
			cluster := Cluster(t.Release, r.Service, r.Port)
			switch {
			case l.Protocol == ProtocolTCP:
				if u.tcp == "" {
					u.tcp = cluster
					if l.ProxyProtocol != "" {
						// Its own cluster: the service port may be exposed elsewhere without the header.
						u.tcp = cluster + "_pp" + l.ProxyProtocol
						cfg.Clusters = append(cfg.Clusters, ports.ProxyCluster{Name: u.tcp, Host: t.IP, Port: r.Port, ProxyProtocol: l.ProxyProtocol})
					}
				}
			case !u.seen[r.Host+r.Path]:
				u.seen[r.Host+r.Path] = true
				u.routes = append(u.routes, ports.ProxyRoute{Host: r.Host, Path: r.Path, Cluster: cluster})
				if _, ok := u.certs[r.Host]; l.Protocol == ProtocolHTTPS && !ok {
					n := mountPath + "/certs/" + certName(t.Release, l.Name, r.Host, r.CertDir)
					u.certs[r.Host] = ports.ProxyCert{Chain: n + ".crt", Key: n + ".key"}
				}
			}
		}
	}
	for _, l := range settings.Listeners {
		u := uses[l.Name]
		var redirects map[string]int
		if l.Protocol == ProtocolHTTP && l.RedirectHTTPS {
			redirects = redirectHosts(settings, uses, u)
		}
		if u == nil && len(redirects) == 0 {
			continue
		}
		lim, _ := l.limits() // validated when the settings were written
		for _, ip := range settings.Listen {
			if l.Protocol == ProtocolTCP {
				cfg.TCP = append(cfg.TCP, ports.ProxyTCPListener{Address: ip, Port: l.Port, Cluster: u.tcp, IdleTimeout: lim.IdleTimeout})
			} else {
				hl := ports.ProxyHTTPListener{Address: ip, Port: l.Port, Redirects: redirects, Limits: lim}
				if u != nil {
					hl.Routes = u.routes
					if l.Protocol == ProtocolHTTPS {
						hl.Certs = u.certs
					}
				}
				cfg.HTTP = append(cfg.HTTP, hl)
			}
		}
	}
	return cfg
}

// redirectHosts returns the hosts served on an https listener but not on the http listener u, each
// with the port of the (first) https listener serving it, for u's redirects to HTTPS.
func redirectHosts(s Settings, uses map[string]*use, u *use) map[string]int {
	var hosts map[string]int
	for _, l := range s.Listeners {
		su := uses[l.Name]
		if l.Protocol != ProtocolHTTPS || su == nil {
			continue
		}
		for _, h := range slices.Sorted(maps.Keys(su.certs)) {
			if _, seen := hosts[h]; seen || u != nil && slices.ContainsFunc(u.routes, func(r ports.ProxyRoute) bool { return r.Host == h }) {
				continue
			}
			if hosts == nil {
				hosts = map[string]int{}
			}
			hosts[h] = l.Port
		}
	}
	return hosts
}

func healthPath(routes []Route, p Port) string {
	for _, r := range routes {
		if r.Service == p.Service && r.Port == p.Port && r.HealthPath != "" {
			return r.HealthPath
		}
	}
	return ""
}

// Want is what the gateway must route to for the given targets to be served: cluster -> endpoint,
// for the clusters its listeners use (a tcp listener with a PROXY header has a cluster of its own).
func Want(settings Settings, targets []Target, keep func(Target) bool) map[string]string {
	var kept []Target
	for _, t := range targets {
		if keep(t) {
			kept = append(kept, t)
		}
	}
	cfg := buildConfig(settings, kept, "")
	used := map[string]bool{}
	for _, l := range cfg.TCP {
		used[l.Cluster] = true
	}
	for _, l := range cfg.HTTP {
		for _, r := range l.Routes {
			used[r.Cluster] = true
		}
	}
	want := map[string]string{}
	for _, c := range cfg.Clusters {
		if used[c.Name] {
			want[c.Name] = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
		}
	}
	return want
}
