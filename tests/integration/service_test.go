//go:build integration

package integration

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
)

// TestServicesAreIsolatedPerRelease: two releases both have a Service web on port 80; each resolves
// to its own workload on its own network.
func TestServicesAreIsolatedPerRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := releaseName("it-iso-a"), releaseName("it-iso-b")
	sa, rt := service(t, a)
	sb, _ := service(t, b)
	for _, x := range []struct {
		s    *releases.Service
		name string
	}{{sa, a}, {sb, b}} {
		if _, err := x.s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: x.name, Package: webPkg,
			Values: releases.Values{Set: []string{"servicePort=80", "content=" + x.name}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{a, b} {
		p := startNetPinger(t, rt, rel, "http://web:80/")
		time.Sleep(2 * time.Second)
		if res := p.result(); res.Bodies[rel] == 0 || len(res.Bodies) != 1 {
			t.Fatalf("%s: Service web should answer with its own content: %s", rel, res)
		}
	}
}

// TestGatewayListenAddresses: the gateway binds only the addresses the administrator sets. Not
// parallel: the listen addresses are host-wide.
func TestGatewayListenAddresses(t *testing.T) {
	ctx := context.Background()
	name := releaseName("it-listen")
	s, _ := service(t, name)
	l, port := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Set: []string{"route=listen.local", "routeListener=" + l}}}, nil); err != nil {
		t.Fatal(err)
	}
	get := func(ip string) (int, error) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(ip, strconv.Itoa(port))+"/", nil)
		req.Host = "listen.local"
		// A fresh connection each time: Envoy lets connections on a removed listener finish.
		resp, err := (&http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := get("127.0.0.1"); err != nil || code != http.StatusOK {
		t.Fatalf("default 127.0.0.1: %d %v", code, err)
	}
	if err := s.Gateway.SetListen(ctx, []string{"127.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Gateway.SetListen(context.Background(), []string{gateway.DefaultListen}) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, err := get("127.0.0.2")
		_, oldErr := get("127.0.0.1")
		if err == nil && code == http.StatusOK && oldErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after set listen 127.0.0.2: new %d %v, old address error %v", code, err, oldErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A listener in use cannot be removed.
	if err := s.Gateway.RemoveListener(ctx, l); err == nil {
		t.Fatal("removed a listener in use")
	}
}
