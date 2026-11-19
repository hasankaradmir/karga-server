//go:build integration

package integration

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// TestEdgeNetwork: a release proxy is on the edge network at its release's address, which the host
// (so the gateway) reaches; another container on the edge network does not: traffic between its
// containers is off, so releases stay apart.
func TestEdgeNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-edge")
	s, rt := service(t, name)
	l, _ := listener(t, gateway.ProtocolHTTP)
	if _, err := s.Lifecycle.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: releases.Values{Sets: packages.Sets([]string{"route=edge.local", "routeListener=" + l}...)}}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := rt.GetWorkload(ctx, proxyName(t, rt, name))
	if err != nil {
		t.Fatal(err)
	}
	ip := st.Labels[gateway.LabelEdgeIP]
	if ip == "" || st.IPs[gateway.EdgeNetwork] != ip {
		t.Fatalf("edge address: label %q, networks %v", ip, st.IPs)
	}
	addr := net.JoinHostPort(ip, "8080")
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		t.Fatalf("the host reaches the proxy at %s: %v", addr, err)
	} else {
		_ = c.Close()
	}

	other := "it-edge-peer-" + strconv.Itoa(int(time.Now().UnixNano()%100000))
	if err := rt.CreateWorkload(ctx, ports.WorkloadRequest{Name: other, Image: "busybox:1.37", RestartPolicy: "never",
		Labels: map[string]string{ports.LabelManaged: "true", ports.LabelRelease: name}, Command: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.DeleteWorkload(context.Background(), other) })
	if err := rt.ConnectNetwork(ctx, other, gateway.EdgeNetwork, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartWorkload(ctx, other); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "exec", other, "wget", "-q", "-T", "3", "-O", "-", "http://"+addr+"/").CombinedOutput()
	if err == nil || strings.Contains(string(out), "v1") {
		t.Fatalf("a container on the edge network reached the proxy at %s: %s", addr, out)
	}
}
