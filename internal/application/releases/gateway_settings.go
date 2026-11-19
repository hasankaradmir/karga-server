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
	return s.c.gw.Routes(ctx)
}

// Settings returns the host's gateway settings and, per listener, the services using it.
func (s *Gateway) Settings(ctx context.Context) (gateway.Settings, map[string][]string, error) {
	settings, err := gateway.ReadSettings(s.c.home)
	if err != nil {
		return settings, nil, err
	}
	users, err := s.c.gw.Users(ctx)
	return settings, users, err
}

// Health reports the gateway container and whether its Envoy is ready.
func (s *Gateway) Health(ctx context.Context) (gateway.State, error) {
	return s.c.gw.Health(ctx)
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

// SetListenerOptions replaces the options of a listener; its port and protocol stay.
func (s *Gateway) SetListenerOptions(ctx context.Context, name string, opts gateway.ListenerOptions) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		i := slices.IndexFunc(g.Listeners, func(l gateway.Listener) bool { return l.Name == name })
		if i < 0 {
			return fmt.Errorf("gateway listener %q: %w", name, ports.ErrNotFound)
		}
		g.Listeners[i].ListenerOptions = opts
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
// to a running gateway, waiting until it routes again; if it does not, the previous settings are
// put back. It refuses while gateway.yaml has edits not applied (D39): they would be lost or
// applied unchecked.
func (s *Gateway) updateGateway(ctx context.Context, change func(*gateway.Settings, map[string][]string) error) error {
	var old gateway.Settings
	err := func() error {
		unlock, err := gateway.Lock(ctx, s.c.home)
		if err != nil {
			return err
		}
		defer unlock()
		if pending, err := gateway.Pending(s.c.home); err != nil || pending {
			return errors.Join(err, fmt.Errorf("%w: gateway.yaml has changes not applied; run 'karga gateway validate' and 'karga gateway apply', or undo them",
				ports.ErrConflict))
		}
		settings, err := gateway.ReadSettings(s.c.home)
		if err != nil {
			return err
		}
		old = settings
		old.Listeners = slices.Clone(settings.Listeners)
		users, err := s.c.gw.Users(ctx)
		if err != nil {
			return err
		}
		if err := change(&settings, users); err != nil {
			return err
		}
		return gateway.WriteSettings(s.c.home, settings)
	}()
	if err != nil {
		return err
	}
	return s.applyOrRevert(ctx, func() error { return gateway.WriteSettings(s.c.home, old) })
}

// ValidateSettings compares gateway.yaml with the settings the gateway runs on.
func (s *Gateway) ValidateSettings(ctx context.Context) (gateway.SettingsPlan, error) {
	unlock, err := gateway.Lock(ctx, s.c.home)
	if err != nil {
		return gateway.SettingsPlan{}, err
	}
	defer unlock()
	return s.c.gw.PlanSettings(ctx)
}

// ApplySettings applies gateway.yaml as validated (digest): a file with problems, changed since,
// or with a risk not confirmed is refused; if the gateway does not route with it, it keeps the
// previous settings. gateway.yaml is left as written either way.
func (s *Gateway) ApplySettings(ctx context.Context, digest string, confirmed bool) error {
	var old gateway.Settings
	err := func() error {
		unlock, err := gateway.Lock(ctx, s.c.home)
		if err != nil {
			return err
		}
		defer unlock()
		plan, err := s.c.gw.PlanSettings(ctx)
		switch {
		case err != nil:
			return err
		case plan.Digest != digest:
			return fmt.Errorf("%w: gateway.yaml changed since it was validated; validate it again", ports.ErrConflict)
		case len(plan.Problems) > 0:
			return fmt.Errorf("%w: gateway.yaml: %s", ports.ErrInvalid, strings.Join(plan.Problems, "; "))
		case plan.Risk != "" && !confirmed:
			return fmt.Errorf("%w: confirmation needed: %s", ports.ErrInvalid, plan.Risk)
		}
		if old, err = gateway.ReadSettings(s.c.home); err != nil {
			return err
		}
		return plan.Apply(s.c.home)
	}()
	if err != nil {
		return err
	}
	return s.applyOrRevert(ctx, func() error { return gateway.WriteApplied(s.c.home, old) })
}

// applyOrRevert syncs a running gateway with the stored settings and waits until it routes; on
// failure revert puts the previous settings back and the gateway is synced with them again.
func (s *Gateway) applyOrRevert(ctx context.Context, revert func() error) error {
	name, err := s.c.gw.Current(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return nil // applied when a release next exposes something
	}
	if err == nil {
		var targets []gateway.Target
		if targets, err = s.c.gw.Sync(ctx); err == nil {
			err = s.c.gw.Wait(ctx, "", targets)
		}
	}
	if err == nil {
		return nil
	}
	err = proxyops.EnvoyErr(ctx, s.c.rt, name, err)
	if rerr := revert(); rerr != nil {
		return errors.Join(err, rerr)
	}
	targets, serr := s.c.gw.Sync(ctx)
	if serr == nil {
		serr = s.c.gw.Wait(ctx, "", targets)
	}
	if serr != nil {
		return fmt.Errorf("%w; the previous settings are back but the gateway did not route with them either: %w", err, serr)
	}
	return fmt.Errorf("%w; the gateway kept the previous settings", err)
}
