package releases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Routes lists what the host gateway serves, across releases.
func (s *Gateway) Routes(ctx context.Context) ([]gateway.Exposed, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	return gateway.Routes(ctx, rt, s.Home)
}

// Settings returns the host's gateway settings and, per listener, the services using it.
func (s *Gateway) Settings(ctx context.Context) (gateway.Settings, map[string][]string, error) {
	settings, err := gateway.ReadSettings(s.Home)
	if err != nil {
		return settings, nil, err
	}
	rt, err := s.Runtime(ctx)
	if err != nil {
		return settings, nil, err
	}
	users, err := gateway.Users(ctx, rt, s.Home)
	return settings, users, err
}

// Health reports the gateway container and whether its Envoy is ready.
func (s *Gateway) Health(ctx context.Context) (gateway.State, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return gateway.State{}, err
	}
	return gateway.Health(ctx, rt, s.Deploy.Host, s.Home)
}

// SetAccessLog stores the access log setting and applies it to a running gateway.
func (s *Gateway) SetAccessLog(ctx context.Context, on bool) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.AccessLog = on
		return nil
	})
}

// SetListen sets the host addresses every gateway listener binds.
func (s *Gateway) SetListen(ctx context.Context, addrs []string) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.Listen = addrs
		return nil
	})
}

// AddListener defines a listener services can expose ports on.
func (s *Gateway) AddListener(ctx context.Context, l gateway.Listener) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.Listeners = append(g.Listeners, l)
		return nil
	})
}

// RemoveListener removes a listener no service uses.
func (s *Gateway) RemoveListener(ctx context.Context, name string) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, users map[string][]string) error {
		i := slices.IndexFunc(g.Listeners, func(l gateway.Listener) bool { return l.Name == name })
		switch {
		case i < 0:
			return fmt.Errorf("gateway listener %q: %w", name, ports.ErrNotFound)
		case len(users[name]) > 0:
			return fmt.Errorf("gateway listener %q is used by %s; remove those exposures first", name, strings.Join(users[name], ", "))
		}
		g.Listeners = slices.Delete(g.Listeners, i, i+1)
		return nil
	})
}

// updateGateway changes the gateway settings under the gateway lock, stores them and applies them
// to a running gateway, waiting until it routes again.
func (s *Gateway) updateGateway(ctx context.Context, change func(*gateway.Settings, map[string][]string) error) error {
	rt, err := s.runtime(ctx)
	if err != nil {
		return err
	}
	err = func() error {
		unlock, err := gateway.Lock(s.Home)
		if err != nil {
			return err
		}
		defer unlock()
		settings, err := gateway.ReadSettings(s.Home)
		if err != nil {
			return err
		}
		users, err := gateway.Users(ctx, rt, s.Home)
		if err != nil {
			return err
		}
		if err := change(&settings, users); err != nil {
			return err
		}
		return gateway.WriteSettings(s.Home, settings)
	}()
	if err != nil {
		return err
	}
	if _, err := rt.GetWorkload(ctx, gateway.ContainerName); errors.Is(err, ports.ErrNotFound) {
		return nil // applied when a release next exposes something
	}
	targets, err := gateway.Sync(ctx, rt, s.Deploy.Host, s.Home)
	if err != nil {
		return err
	}
	want := gateway.Want(targets, func(t gateway.Target) bool { return len(t.Routes) > 0 })
	if len(want) == 0 {
		return nil
	}
	return s.Deploy.Proxies.WaitReady(ctx, gateway.Dir(s.Home), want, proxyops.ProxyReadyTimeout)
}
