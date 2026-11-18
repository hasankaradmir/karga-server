package envoy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/stream/v3"
	dnscommonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/common/dns/v3"
	dnsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/dns/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	tlsinspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Listeners and clusters are built as Envoy's own protobuf types and validated with the
// constraints Envoy itself enforces, so a mistake fails here with a clear message instead of
// Envoy silently rejecting the file.

// validated is a generated message with protoc-gen-validate rules.
type validated interface {
	proto.Message
	ValidateAll() error
}

// pack validates m and wraps it for a typed_config field.
func pack(m validated) (*anypb.Any, error) {
	if err := m.ValidateAll(); err != nil {
		return nil, fmt.Errorf("envoy config: %w", err)
	}
	return anypb.New(m)
}

func socketAddress(host string, port int) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Address: host, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(port)},
	}}}
}

func filterListener(name, address string, port int, filter string, cfg validated) (*listenerv3.Listener, error) {
	tc, err := pack(cfg)
	if err != nil {
		return nil, err
	}
	return &listenerv3.Listener{
		Name:    name,
		Address: socketAddress(address, port),
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{{
			Name: filter, ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: tc},
		}}}},
	}, nil
}

// Listeners renders lds.yaml. Output is sorted so equal configs give equal files.
func Listeners(c Config) ([]byte, error) {
	var items []validated
	var logs []*accesslogv3.AccessLog
	if c.AccessLog {
		stdout, err := pack(&streamv3.StdoutAccessLog{})
		if err != nil {
			return nil, err
		}
		logs = []*accesslogv3.AccessLog{{Name: "envoy.access_loggers.stdout", ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: stdout}}}
	}
	tcp := slices.Clone(c.TCP)
	slices.SortFunc(tcp, func(a, b TCPListener) int {
		return strings.Compare(tcpName(a.Address, a.Port), tcpName(b.Address, b.Port))
	})
	for _, l := range tcp {
		name := tcpName(l.Address, l.Port)
		lis, err := filterListener(name, l.Address, l.Port, "envoy.filters.network.tcp_proxy", &tcpv3.TcpProxy{
			StatPrefix:         name,
			ClusterSpecifier:   &tcpv3.TcpProxy_Cluster{Cluster: l.Cluster},
			MaxConnectAttempts: wrapperspb.UInt32(3),
			AccessLog:          logs,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, lis)
	}
	httpls := slices.Clone(c.HTTP)
	slices.SortFunc(httpls, func(a, b HTTPListener) int {
		return strings.Compare(httpName(a.Address, a.Port), httpName(b.Address, b.Port))
	})
	router, err := pack(&routerv3.Router{})
	if err != nil {
		return nil, err
	}
	for _, l := range httpls {
		lis, err := httpListener(l, router, logs)
		if err != nil {
			return nil, err
		}
		items = append(items, lis)
	}
	return resources(items)
}

func hcm(name string, router *anypb.Any, hosts []*routev3.VirtualHost, logs []*accesslogv3.AccessLog) *hcmv3.HttpConnectionManager {
	return &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		AccessLog:  logs,
		// The gateway sees the real client address and passes it on in x-forwarded-for.
		UseRemoteAddress: wrapperspb.Bool(true),
		// Routes of different releases differ by path prefix: /a/../b or //b must not reach b's
		// routes in another form than a plain /b would.
		NormalizePath: wrapperspb.Bool(true),
		MergeSlashes:  true,
		HttpFilters: []*hcmv3.HttpFilter{{
			Name: "envoy.filters.http.router", ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: router},
		}},
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name: name, VirtualHosts: hosts,
		}},
	}
}

// httpListener renders a plain listener (one filter chain) or a TLS one (a chain per host,
// selected by SNI, each with its certificate and its routes).
func httpListener(l HTTPListener, router *anypb.Any, logs []*accesslogv3.AccessLog) (*listenerv3.Listener, error) {
	name := httpName(l.Address, l.Port)
	if len(l.Certs) == 0 {
		hosts := virtualHosts(l.Routes)
		for _, h := range slices.Sorted(slices.Values(l.Redirects)) {
			hosts = append(hosts, redirectHost(h, l.RedirectPort))
		}
		return filterListener(name, l.Address, l.Port, "envoy.filters.network.http_connection_manager", hcm(name, router, hosts, logs))
	}
	inspector, err := pack(&tlsinspectorv3.TlsInspector{})
	if err != nil {
		return nil, err
	}
	lis := &listenerv3.Listener{
		Name:    name,
		Address: socketAddress(l.Address, l.Port),
		ListenerFilters: []*listenerv3.ListenerFilter{{
			Name: "envoy.filters.listener.tls_inspector", ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: inspector},
		}},
	}
	for _, host := range slices.Sorted(maps.Keys(l.Certs)) {
		var routes []HTTPRoute
		for _, r := range l.Routes {
			if r.Host == host {
				routes = append(routes, r)
			}
		}
		cert := l.Certs[host]
		tlsCtx, err := pack(&tlsv3.DownstreamTlsContext{CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsCertificates: []*tlsv3.TlsCertificate{{
				CertificateChain: &corev3.DataSource{Specifier: &corev3.DataSource_Filename{Filename: cert.Chain}},
				PrivateKey:       &corev3.DataSource{Specifier: &corev3.DataSource_Filename{Filename: cert.Key}},
			}},
		}})
		if err != nil {
			return nil, err
		}
		manager, err := pack(hcm(name, router, virtualHosts(routes), logs))
		if err != nil {
			return nil, err
		}
		lis.FilterChains = append(lis.FilterChains, &listenerv3.FilterChain{
			FilterChainMatch: &listenerv3.FilterChainMatch{ServerNames: []string{host}},
			TransportSocket: &corev3.TransportSocket{
				Name: "envoy.transport_sockets.tls", ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: tlsCtx},
			},
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.http_connection_manager", ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: manager},
			}},
		})
	}
	return lis, nil
}

// redirectHost answers every plain request for host with a redirect to HTTPS.
func redirectHost(host string, port int) *routev3.VirtualHost {
	redirect := &routev3.RedirectAction{SchemeRewriteSpecifier: &routev3.RedirectAction_HttpsRedirect{HttpsRedirect: true}}
	if port != 443 {
		redirect.PortRedirect = uint32(port)
	}
	return &routev3.VirtualHost{
		Name: host + "-https", Domains: []string{host, host + ":*"},
		Routes: []*routev3.Route{{
			Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
			Action: &routev3.Route_Redirect{Redirect: redirect},
		}},
	}
}

// virtualHosts groups routes by host; within a host longer path prefixes match first.
func virtualHosts(routes []HTTPRoute) []*routev3.VirtualHost {
	byHost := map[string][]HTTPRoute{}
	for _, r := range routes {
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	var out []*routev3.VirtualHost
	for _, h := range slices.Sorted(maps.Keys(byHost)) {
		rs := byHost[h]
		slices.SortFunc(rs, func(a, b HTTPRoute) int {
			if len(a.Path) != len(b.Path) {
				return len(b.Path) - len(a.Path)
			}
			return strings.Compare(a.Path, b.Path)
		})
		vh := &routev3.VirtualHost{Name: h, Domains: []string{h, h + ":*"}}
		for _, r := range rs {
			vh.Routes = append(vh.Routes, &routev3.Route{
				Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: r.Path}},
				Action: &routev3.Route_Route{Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: r.Cluster},
					// Only connect failures are retried: the request has not reached a backend yet.
					RetryPolicy: &routev3.RetryPolicy{RetryOn: "connect-failure", NumRetries: wrapperspb.UInt32(2)},
				}},
			})
		}
		out = append(out, vh)
	}
	return out
}

// Clusters renders cds.yaml.
func Clusters(c Config) ([]byte, error) {
	cs := slices.Clone(c.Clusters)
	slices.SortFunc(cs, func(a, b Cluster) int { return strings.Compare(a.Name, b.Name) })
	var items []validated
	for _, cl := range cs {
		item := &clusterv3.Cluster{
			Name:           cl.Name,
			ConnectTimeout: durationpb.New(time.Second),
			LoadAssignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: cl.Name,
				Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(cl.Host, cl.Port)}},
				}}}},
			},
			// A new cluster only takes traffic once its endpoint passed this check (warming).
			HealthChecks: []*corev3.HealthCheck{{
				Timeout:  durationpb.New(time.Second),
				Interval: durationpb.New(500 * time.Millisecond),
				// Without these Envoy checks a cluster that has no traffic yet, or a host that failed,
				// only every 60s: a new backend whose first check failed stayed out for a minute.
				NoTrafficInterval: durationpb.New(500 * time.Millisecond),
				UnhealthyInterval: durationpb.New(500 * time.Millisecond),
				// Three failed checks in a row take a backend out: one slow answer does not flap it.
				UnhealthyThreshold: wrapperspb.UInt32(3),
				HealthyThreshold:   wrapperspb.UInt32(1),
				HealthChecker:      &corev3.HealthCheck_TcpHealthCheck_{TcpHealthCheck: &corev3.HealthCheck_TcpHealthCheck{}},
			}},
		}
		if cl.HealthPath != "" {
			item.HealthChecks[0].HealthChecker = &corev3.HealthCheck_HttpHealthCheck_{HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
				Path:             cl.HealthPath,
				ExpectedStatuses: []*typev3.Int64Range{{Start: 200, End: 400}},
			}}
		}
		if cl.DNS {
			// The DnsCluster extension replaces the deprecated type/dns_refresh_rate fields.
			dns, err := pack(&dnsv3.DnsCluster{
				DnsRefreshRate:  durationpb.New(time.Second),
				DnsLookupFamily: dnscommonv3.DnsLookupFamily_V4_ONLY,
			})
			if err != nil {
				return nil, err
			}
			item.ClusterDiscoveryType = &clusterv3.Cluster_ClusterType{ClusterType: &clusterv3.Cluster_CustomClusterType{
				Name: "envoy.cluster.dns", TypedConfig: dns,
			}}
		} else {
			item.ClusterDiscoveryType = &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC}
		}
		items = append(items, item)
	}
	return resources(items)
}

// resources validates items and writes them as a discovery response, the file format of
// path_config_source. protojson output is normalized (it varies whitespace on purpose) so equal
// configs give equal files.
func resources(items []validated) ([]byte, error) {
	resp := &discoveryv3.DiscoveryResponse{}
	for _, it := range items {
		if err := it.ValidateAll(); err != nil {
			return nil, fmt.Errorf("envoy config: %w", err)
		}
		a, err := anypb.New(it)
		if err != nil {
			return nil, err
		}
		resp.Resources = append(resp.Resources, a)
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
	if err != nil {
		return nil, err
	}
	if len(resp.Resources) == 0 {
		raw = []byte(`{"resources":[]}`)
	}
	// Envoy's own JSON mapping, stored as YAML.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return toYAML(numbers(doc))
}

// numbers turns JSON numbers into ints or floats, so YAML writes them as numbers.
func numbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = numbers(c)
		}
	case []any:
		for i, c := range t {
			t[i] = numbers(c)
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}
