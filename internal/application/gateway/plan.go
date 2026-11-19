package gateway

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// SettingsPlan is what applying a gateway document would change (D40). Problems refuse it; Risk
// needs the user's confirmation; Settings are what it applies.
type SettingsPlan struct {
	Changes  []SettingsChange
	Problems []string
	Risk     string
	Settings Settings
}

// SettingsChange is one difference: Kind "listener", "listen" or "accessLog"; Action "add",
// "remove" or "change".
type SettingsChange struct {
	Kind, Name, Action string
	Fields             []string
}

// PlanDocument is PlanSettings for a document as the user wrote it: one that does not parse is a
// problem.
func (g *Gateway) PlanDocument(ctx context.Context, content []byte) (SettingsPlan, error) {
	edited, err := ParseDocument(content)
	if err != nil {
		return SettingsPlan{Problems: []string{err.Error()}}, nil
	}
	return g.PlanSettings(ctx, edited)
}

// PlanSettings compares edited settings with the ones in force, under the gateway lock: they must
// validate, the listeners Services use must stay with their protocol, and a port the gateway
// starts to serve must be free.
func (g *Gateway) PlanSettings(ctx context.Context, edited Settings) (SettingsPlan, error) {
	applied, err := ReadSettings(g.home)
	if err != nil {
		return SettingsPlan{}, err
	}
	plan := SettingsPlan{Settings: edited}
	if err := validateSettings(edited); err != nil {
		_, msg, _ := strings.Cut(err.Error(), ": ") // without the "invalid: " prefix
		plan.Problems = append(plan.Problems, strings.Split(msg, "\n")...)
	}
	users, err := g.Users(ctx)
	if err != nil {
		return plan, err
	}
	plan.Changes = settingsChanges(applied, edited)
	var risks []string
	for _, l := range applied.Listeners {
		n, ok := listenerOf(edited, l.Name)
		switch {
		case !ok && len(users[l.Name]) > 0:
			plan.Problems = append(plan.Problems, fmt.Sprintf("listener %s is used by %s; remove those exposures first", l.Name, strings.Join(users[l.Name], ", ")))
		case !ok:
			risks = append(risks, "this removes listener "+l.Name)
		case n.Protocol != l.Protocol && len(users[l.Name]) > 0:
			plan.Problems = append(plan.Problems, fmt.Sprintf("listener %s is used by %s; its protocol cannot change", l.Name, strings.Join(users[l.Name], ", ")))
		}
	}
	// A listener in use opens where it was not open: nothing else may hold the port.
	for _, n := range edited.Listeners {
		if len(users[n.Name]) == 0 {
			continue
		}
		old, had := listenerOf(applied, n.Name)
		for _, ip := range edited.Listen {
			if had && old.Port == n.Port && slices.Contains(applied.Listen, ip) {
				continue
			}
			if taken, err := g.host.Net.PortTaken("tcp", ip, n.Port); err == nil && taken {
				plan.Problems = append(plan.Problems, fmt.Sprintf("listener %s: %s is taken by another process on this host", n.Name, net.JoinHostPort(ip, strconv.Itoa(n.Port))))
			}
		}
	}
	var open []string
	for _, ip := range edited.Listen {
		if a := net.ParseIP(ip); a != nil && !a.IsLoopback() && !slices.Contains(applied.Listen, ip) {
			open = append(open, ip)
		}
	}
	if len(open) > 0 {
		risks = append(risks, fmt.Sprintf("every exposed service becomes reachable on %s, from other machines too", strings.Join(open, ", ")))
	}
	plan.Risk = strings.Join(risks, "; ")
	// Envoy checks what karga's own checks passed (a certificate it cannot load, a limit it refuses).
	if len(plan.Problems) == 0 && len(plan.Changes) > 0 && g.host.Supported() {
		why, err := g.validateEnvoy(ctx, edited)
		if err != nil {
			return plan, err
		}
		if why != "" {
			plan.Problems = append(plan.Problems, why)
		}
	}
	return plan, nil
}

// settingsChanges lists what differs between the applied settings and the edited ones.
func settingsChanges(applied, edited Settings) []SettingsChange {
	var out []SettingsChange
	if !slices.Equal(applied.Listen, edited.Listen) {
		out = append(out, SettingsChange{Kind: "listen", Action: "change",
			Fields: []string{strings.Join(applied.Listen, ",") + " -> " + strings.Join(edited.Listen, ",")}})
	}
	if applied.AccessLog != edited.AccessLog {
		out = append(out, SettingsChange{Kind: "accessLog", Action: "change", Fields: []string{onOff(applied.AccessLog) + " -> " + onOff(edited.AccessLog)}})
	}
	for _, l := range applied.Listeners {
		n, ok := listenerOf(edited, l.Name)
		if !ok {
			out = append(out, SettingsChange{Kind: "listener", Name: l.Name, Action: "remove"})
			continue
		}
		var fields []string
		for _, f := range listenerFields {
			if a, b := f.get(l), f.get(n); a != b {
				fields = append(fields, fmt.Sprintf("%s: %s -> %s", f.name, dash(a), dash(b)))
			}
		}
		if len(fields) > 0 {
			out = append(out, SettingsChange{Kind: "listener", Name: l.Name, Action: "change", Fields: fields})
		}
	}
	for _, n := range edited.Listeners {
		if _, ok := listenerOf(applied, n.Name); ok {
			continue
		}
		var fields []string
		for _, f := range listenerFields {
			if v := f.get(n); v != "" {
				fields = append(fields, f.name+": "+v)
			}
		}
		out = append(out, SettingsChange{Kind: "listener", Name: n.Name, Action: "add", Fields: fields})
	}
	return out
}

var listenerFields = []struct {
	name string
	get  func(Listener) string
}{
	{"port", func(l Listener) string { return strconv.Itoa(l.Port) }},
	{"protocol", func(l Listener) string { return l.Protocol }},
	{"timeout", func(l Listener) string { return l.Timeout }},
	{"idleTimeout", func(l Listener) string { return l.IdleTimeout }},
	{"maxBody", func(l Listener) string { return l.MaxBody }},
	{"rateLimit", func(l Listener) string { return l.RateLimit }},
	{"proxyProtocol", func(l Listener) string { return l.ProxyProtocol }},
	{"redirectHTTPS", func(l Listener) string {
		if l.RedirectHTTPS {
			return "on"
		}
		return ""
	}},
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
