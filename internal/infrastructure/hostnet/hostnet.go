// Package hostnet implements ports.HostNet with the host's network stack.
package hostnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"syscall"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Net is the host's network.
type Net struct{}

var _ ports.HostNet = Net{}

// Probe implements ports.HostNet.
func (Net) Probe(ctx context.Context, p ports.Probe) error {
	if p.Type == "tcp" {
		c, err := (&net.Dialer{Timeout: p.Timeout}).DialContext(ctx, "tcp", p.Addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.Addr+p.Path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s", p.Path, resp.Status)
	}
	return nil
}

// PortTaken implements ports.HostNet. karga-server runs without privileges: binding a port below
// 1024 fails with EACCES, and the port is then reported as not tested (Docker binds the gateway's
// ports as root).
func (Net) PortTaken(protocol, ip string, port int) (taken bool, err error) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	if protocol == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			_ = c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			_ = l.Close()
		}
	}
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return true, nil
	case errors.Is(err, syscall.EACCES):
		return false, err
	}
	return false, nil
}
