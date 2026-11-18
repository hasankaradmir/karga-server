package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/runtimeops"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// tcp and http health checks are run by karga itself, from the host, so the image needs no tool:
// karga connects to the container's network address, which needs Linux with a local Docker.

// readyTarget is a container to wait for; spec decides how its health is checked.
type readyTarget struct {
	name string
	spec *resource.WorkloadSpec
}

func kargaProbes(s *resource.WorkloadSpec) bool {
	return s != nil && s.HealthCheck != nil && s.HealthCheck.Type != resource.HealthExec
}

// probeReachable says why karga could not reach the health check port of s, or "".
func probeReachable(s *resource.WorkloadSpec) string {
	if !kargaProbes(s) || runtimeops.Supported.Load() {
		return ""
	}
	return fmt.Sprintf("healthCheck type %s needs Linux with a local Docker (karga checks the container's address); use type exec", s.HealthCheck.Type)
}

// probeAddr is where karga reaches the health check port of a running container.
func probeAddr(st ports.WorkloadStatus, s *resource.WorkloadSpec) (string, error) {
	port := s.HealthCheck.Port
	if !runtimeops.Supported.Load() {
		return "", errors.New(probeReachable(s))
	}
	for _, n := range slices.Sorted(maps.Keys(st.IPs)) {
		return net.JoinHostPort(st.IPs[n], strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("workload %s has no network address to check port %d on", st.Name, port)
}

func durationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// probe runs one tcp or http check against addr.
func probe(ctx context.Context, hc *resource.HealthCheck, addr string) error {
	timeout := durationOr(hc.Timeout, 2*time.Second)
	if hc.Type == resource.HealthTCP {
		c, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+hc.Path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s", hc.Path, resp.Status)
	}
	return nil
}

// ProbeHealth checks a running workload once: "healthy", "unhealthy" or "unknown" (unreachable).
func ProbeHealth(ctx context.Context, st ports.WorkloadStatus, s *resource.WorkloadSpec) string {
	if !kargaProbes(s) || st.State != ports.StateRunning {
		return st.Health
	}
	addr, err := probeAddr(st, s)
	if err != nil {
		return "unknown"
	}
	if probe(ctx, s.HealthCheck, addr) != nil {
		return ports.HealthUnhealthy
	}
	return ports.HealthHealthy
}

// probeState tracks one karga-checked target while waiting.
type probeState struct {
	runningSince time.Time
	next         time.Time
	failures     int
	lastErr      error
}

// waitReady waits until every target is ready: healthy by Docker (exec) or by karga's check (tcp,
// http), or running without a health check. A karga check is tried every interval once the
// container runs; after startPeriod, retries failures in a row make it unhealthy. It returns the
// targets that did not become ready.
func waitReady(ctx context.Context, rt ports.Runtime, targets []readyTarget, timeout time.Duration) ([]string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	pending := slices.Clone(targets)
	probes := map[string]*probeState{}
	names := func(ts []readyTarget) []string {
		var out []string
		for _, t := range ts {
			out = append(out, t.name)
		}
		return out
	}
	var last ports.WorkloadStatus
	start, lastReport := time.Now(), time.Now()
	for {
		var still []readyTarget
		report := time.Since(lastReport) >= waitEvery
		if report {
			lastReport = time.Now()
		}
		for _, t := range pending {
			st, err := rt.GetWorkload(ctx, t.name)
			if err != nil {
				return names(pending), err
			}
			switch {
			case st.State == ports.StateExited || st.State == "dead":
				return names(pending), fmt.Errorf("workload %s exited with code %d before becoming ready", t.name, st.ExitCode)
			case st.Health == ports.HealthUnhealthy:
				return names(pending), fmt.Errorf("workload %s is unhealthy", t.name)
			case st.State == ports.StateRunning && kargaProbes(t.spec):
				ready, err := checkProbe(ctx, st, t.spec, probes, t.name)
				if err != nil {
					return names(pending), err
				}
				if !ready {
					still, last = append(still, t), st
				}
			case st.State == ports.StateRunning && (st.Health == ports.HealthHealthy || st.Health == ports.HealthNone):
			default:
				still, last = append(still, t), st
			}
		}
		if pending = still; len(pending) == 0 {
			return nil, nil
		}
		if report {
			for _, t := range pending {
				why := "starting"
				if ps := probes[t.name]; ps != nil && ps.lastErr != nil {
					why = ps.lastErr.Error()
				} else if t.name == last.Name && last.Health != "" && last.Health != ports.HealthNone {
					why = "health " + last.Health
				}
				progress(ctx, "Workload", t.name, PhaseWait, "waiting for %s to be ready (%s, %s)", t.name, why, time.Since(start).Round(time.Second))
			}
		}
		select {
		case <-ctx.Done():
			return names(pending), ctx.Err()
		case <-deadline.C:
			if ps := probes[last.Name]; ps != nil && ps.lastErr != nil {
				return names(pending), fmt.Errorf("workload %s not ready after %s: %w", last.Name, timeout, ps.lastErr)
			}
			return names(pending), fmt.Errorf("workload %s not ready after %s (state %q, health %q)", last.Name, timeout, last.State, last.Health)
		case <-time.After(pollInterval):
		}
	}
}

func checkProbe(ctx context.Context, st ports.WorkloadStatus, s *resource.WorkloadSpec, probes map[string]*probeState, name string) (bool, error) {
	hc := s.HealthCheck
	now := time.Now()
	ps := probes[name]
	if ps == nil {
		ps = &probeState{runningSince: now}
		probes[name] = ps
	}
	if now.Before(ps.next) {
		return false, nil
	}
	ps.next = now.Add(durationOr(hc.Interval, 2*time.Second))
	addr, err := probeAddr(st, s)
	if err != nil {
		return false, err
	}
	if ps.lastErr = probe(ctx, hc, addr); ps.lastErr == nil {
		return true, nil
	}
	if now.Sub(ps.runningSince) < durationOr(hc.StartPeriod, 0) {
		return false, nil // failures in the start period do not count
	}
	ps.failures++
	retries := hc.Retries
	if retries == 0 {
		retries = 3
	}
	if ps.failures >= retries {
		return false, fmt.Errorf("workload %s is unhealthy: %s check on %s failed %d times: %w", name, hc.Type, addr, ps.failures, ps.lastErr)
	}
	return false, nil
}
