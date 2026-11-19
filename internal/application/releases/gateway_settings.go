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
	return s.updateGateway(ctx, "set access-log "+map[bool]string{true: "on", false: "off"}[on], func(g *gateway.Settings, _ map[string][]string) error {
		g.AccessLog = on
		return nil
	})
}

// SetListen sets the host addresses every gateway listener binds.
func (s *Gateway) SetListen(ctx context.Context, addrs []string) error {
	return s.updateGateway(ctx, "set listen "+strings.Join(addrs, ","), func(g *gateway.Settings, _ map[string][]string) error {
		g.Listen = addrs
		return nil
	})
}

// AddListener defines a listener services can expose ports on.
func (s *Gateway) AddListener(ctx context.Context, l gateway.Listener) error {
	return s.updateGateway(ctx, "listener add "+l.Name, func(g *gateway.Settings, _ map[string][]string) error {
		g.Listeners = append(g.Listeners, l)
		return nil
	})
}

// SetListenerOptions replaces the options of a listener; its port and protocol stay.
func (s *Gateway) SetListenerOptions(ctx context.Context, name string, opts gateway.ListenerOptions) error {
	return s.updateGateway(ctx, "listener set "+name, func(g *gateway.Settings, _ map[string][]string) error {
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
	return s.updateGateway(ctx, "listener remove "+name, func(g *gateway.Settings, users map[string][]string) error {
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

// updateGateway changes the settings in force and applies them as a new revision (source says
// what changed them for the history). The command asked for its own confirmation (Risk).
func (s *Gateway) updateGateway(ctx context.Context, source string, change func(*gateway.Settings, map[string][]string) error) error {
	settings, err := gateway.ReadSettings(s.c.home)
	if err != nil {
		return err
	}
	settings.Listen = slices.Clone(settings.Listen)
	settings.Listeners = slices.Clone(settings.Listeners)
	users, err := s.c.gw.Users(ctx)
	if err != nil {
		return err
	}
	if err := change(&settings, users); err != nil {
		return err
	}
	_, err = s.applySettings(ctx, settings, source, true)
	return err
}

// Diff compares a gateway document with the settings in force (D40); it changes nothing.
func (s *Gateway) Diff(ctx context.Context, content []byte) (gateway.SettingsPlan, error) {
	unlock, err := gateway.Lock(ctx, s.c.home)
	if err != nil {
		return gateway.SettingsPlan{}, err
	}
	defer unlock()
	return s.c.gw.PlanDocument(ctx, content)
}

// Apply applies a gateway document as a new revision; see applySettings.
func (s *Gateway) Apply(ctx context.Context, content []byte, source string, confirmed bool) (int, error) {
	settings, err := gateway.ParseDocument(content)
	if err != nil {
		return 0, fmt.Errorf("%w: gateway document: %w", ports.ErrInvalid, err)
	}
	if source == "" {
		source = "apply"
	}
	return s.applySettings(ctx, settings, source, confirmed)
}

// Export returns revision n of the settings as a document (0: the one in force).
func (s *Gateway) Export(n int) ([]byte, error) {
	settings, _, err := gateway.RevisionSettings(s.c.home, n)
	if err != nil {
		return nil, err
	}
	return gateway.Document(settings), nil
}

// History lists the settings revisions kept, oldest first.
func (s *Gateway) History() ([]gateway.SettingsRevision, error) { return gateway.Revisions(s.c.home) }

// applySettings applies settings as the next revision: settings with problems, or with a risk
// not confirmed, are refused; settings that change nothing write no revision. If the gateway does
// not route with them, the revision is dropped and the one before keeps serving. It returns the
// revision in force.
func (s *Gateway) applySettings(ctx context.Context, settings gateway.Settings, source string, confirmed bool) (int, error) {
	var rev, prev int
	changed := false
	err := func() error {
		unlock, err := gateway.Lock(ctx, s.c.home)
		if err != nil {
			return err
		}
		defer unlock()
		plan, err := s.c.gw.PlanSettings(ctx, settings)
		switch {
		case err != nil:
			return err
		case len(plan.Problems) > 0:
			return fmt.Errorf("%w: gateway settings: %s", ports.ErrInvalid, strings.Join(plan.Problems, "; "))
		case plan.Risk != "" && !confirmed:
			return fmt.Errorf("%w: confirmation needed: %s", ports.ErrInvalid, plan.Risk)
		}
		if _, prev, err = gateway.RevisionSettings(s.c.home, 0); err != nil {
			return err
		}
		if len(plan.Changes) == 0 {
			return nil
		}
		changed = true
		rev, err = gateway.WriteRevision(s.c.home, settings, actorOf(ctx), source)
		return err
	}()
	if err != nil || !changed {
		return prev, err
	}
	if err := s.applyOrRevert(ctx, prev, func() error { return gateway.DropRevision(s.c.home, rev) }); err != nil {
		return prev, err
	}
	return rev, nil
}

// applyOrRevert syncs a running gateway with the settings in force and waits until it routes; on
// failure revert drops the new revision and the gateway is synced with revision prev again.
func (s *Gateway) applyOrRevert(ctx context.Context, prev int, revert func() error) error {
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
		return fmt.Errorf("%w; revision %d is back but the gateway did not route with it either: %w", err, prev, serr)
	}
	return fmt.Errorf("%w; the gateway kept revision %d", err, prev)
}
