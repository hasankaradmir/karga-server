package releases

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/gateway"
)

// Logs writes container logs to w. Several containers are prefixed "name | " per line; without
// Follow they are shown one after the other, with Follow side by side until ctx is done.
func (s *Service) Logs(ctx context.Context, req LogsRequest, w io.Writer) error {
	containers, err := s.logTargets(ctx, req)
	if err != nil {
		return err
	}
	rt, err := s.Runtime(ctx)
	if err != nil {
		return err
	}
	if len(containers) == 1 {
		return rt.Logs(ctx, containers[0].runtime, req.Tail, req.Follow, w)
	}
	var mu sync.Mutex
	run := func(c logTarget) error {
		pw := &prefixWriter{w: w, mu: &mu, prefix: c.label + " | "}
		err := rt.Logs(ctx, c.runtime, req.Tail, req.Follow, pw)
		pw.flush()
		return err
	}
	if !req.Follow {
		var errs []error
		for _, c := range containers {
			errs = append(errs, run(c))
		}
		return errors.Join(errs...)
	}
	errs := make([]error, len(containers))
	var wg sync.WaitGroup
	for i, c := range containers {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = run(c) }()
	}
	wg.Wait()
	return errors.Join(errs...)
}

type logTarget struct{ label, runtime string }

func (s *Service) logTargets(ctx context.Context, req LogsRequest) ([]logTarget, error) {
	if req.Gateway {
		return []logTarget{{"gateway", gateway.ContainerName}}, nil
	}
	if _, err := s.Store.GetRelease(ctx, req.Release); err != nil {
		return nil, err
	}
	applied, err := s.Store.ListResources(ctx, req.Release)
	if err != nil {
		return nil, err
	}
	var all []logTarget
	var proxy *logTarget
	for _, a := range applied {
		switch {
		case a.Kind == resource.KindWorkload:
			all = append(all, logTarget{a.Name, a.RuntimeName})
		case a.Kind == deployment.KindProxy && a.Name == deployment.ProxyResource:
			proxy = &logTarget{"proxy", a.RuntimeName}
		}
	}
	slices.SortFunc(all, func(a, b logTarget) int { return strings.Compare(a.label, b.label) })
	if req.Proxy {
		if proxy == nil {
			return nil, fmt.Errorf("release %s has no proxy", req.Release)
		}
		return []logTarget{*proxy}, nil
	}
	if len(req.Workloads) == 0 {
		if len(all) == 0 {
			return nil, fmt.Errorf("release %s has no workloads", req.Release)
		}
		return all, nil
	}
	var out []logTarget
	for _, n := range req.Workloads {
		i := slices.IndexFunc(all, func(t logTarget) bool { return t.label == n })
		if i < 0 {
			var names []string
			for _, t := range all {
				names = append(names, t.label)
			}
			return nil, fmt.Errorf("release %s has no workload %q (workloads: %s)", req.Release, n, strings.Join(names, ", "))
		}
		out = append(out, all[i])
	}
	return out, nil
}

// prefixWriter writes complete lines to w with a prefix, holding mu so lines of several
// containers do not interleave.
type prefixWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		if err := p.line(p.buf[:i+1]); err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
}

func (p *prefixWriter) flush() {
	if len(p.buf) > 0 {
		_ = p.line(append(p.buf, '\n'))
		p.buf = nil
	}
}

func (p *prefixWriter) line(l []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := io.WriteString(p.w, p.prefix+string(l))
	return err
}
