// Package reconciler decides when karga-server reconciles a release: on Docker events, on a timer
// and when told to. What a reconciliation does is releases.Service.Reconcile.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Loop reconciles releases one at a time, so it costs nothing while all is well.
type Loop struct {
	Svc *releases.Service
	// Interval between full passes over every release; 0 reacts to events only.
	Interval time.Duration
	Log      *slog.Logger
	// Only, if set, limits the loop to the releases it accepts (tests sharing a host).
	Only func(release string) bool
	// Debounce collects the events of a release before it is reconciled (a recreate fires several).
	Debounce time.Duration

	mu      sync.Mutex
	pending map[string]*time.Timer
	retries map[string]*time.Timer // repairs waiting out their backoff
	queue   chan string            // release names; "" means every release
}

// New returns a loop for svc.
func New(svc *releases.Service, interval time.Duration, log *slog.Logger) *Loop {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Loop{Svc: svc, Interval: interval, Log: log, Debounce: 2 * time.Second, pending: map[string]*time.Timer{}, retries: map[string]*time.Timer{}, queue: make(chan string, 256)}
}

// Notify asks for a reconciliation of release name after Debounce ("" means every release).
func (l *Loop) Notify(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.pending[name]; ok {
		t.Reset(l.Debounce)
		return
	}
	l.pending[name] = time.AfterFunc(l.Debounce, func() {
		l.mu.Lock()
		delete(l.pending, name)
		l.mu.Unlock()
		select {
		case l.queue <- name:
		default: // the queue is full of work already; the next pass covers it
		}
	})
}

// Run reconciles until ctx is done: every release at start (after a reboot, say), after events
// of a release's containers, and every Interval.
func (l *Loop) Run(ctx context.Context) {
	go l.watch(ctx)
	var tick <-chan time.Time
	if l.Interval > 0 {
		t := time.NewTicker(l.Interval)
		defer t.Stop()
		tick = t.C
	}
	l.all(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			l.all(ctx)
		case name := <-l.queue:
			if name == "" {
				l.all(ctx)
			} else {
				l.one(ctx, name)
			}
		}
	}
}

func (l *Loop) all(ctx context.Context) {
	rels, err := l.Svc.List(ctx)
	if err != nil {
		l.Log.Warn("reconcile: list releases", "error", err)
		return
	}
	for _, r := range rels {
		if ctx.Err() != nil {
			return
		}
		l.one(ctx, r.Name)
	}
}

func (l *Loop) one(ctx context.Context, name string) {
	if l.Only != nil && !l.Only(name) {
		return
	}
	// A repair that started finishes even when the server stops.
	res, err := l.Svc.Reconcile(context.WithoutCancel(ctx), name, false)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return // a container of another KARGA_HOME, or a release uninstalled meanwhile
	case err != nil:
		l.Log.Warn("reconcile", "release", name, "error", err)
		return
	case len(res.Actions) > 0:
		for _, a := range res.Actions {
			l.Log.Info("reconciled", "release", name, "resource", a.Kind+"/"+a.Name, "action", a.Action, "status", a.Status, "error", a.Error)
		}
	}
	for what, why := range res.GaveUp {
		l.Log.Warn("reconcile gave up", "release", name, "resource", what, "error", why)
	}
	// A repair waiting out its backoff is retried when it may run, events or not.
	if at := l.Svc.NextRepair(name); !at.IsZero() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.retries[name]; !ok {
			l.retries[name] = time.AfterFunc(time.Until(at), func() {
				l.mu.Lock()
				delete(l.retries, name)
				l.mu.Unlock()
				l.Notify(name)
			})
		}
	}
}

// watch turns the runtime's container events into reconciliations, reconnecting when the stream
// breaks (Docker restarted).
func (l *Loop) watch(ctx context.Context) {
	for ctx.Err() == nil {
		rt, err := l.Svc.Runtime(ctx)
		var events <-chan ports.RuntimeEvent
		if err == nil {
			events, err = rt.Events(ctx)
		}
		if err != nil {
			l.Log.Warn("reconcile: runtime events", "error", err)
		} else {
			for e := range events {
				if e.Release != "" && e.Action != "start" {
					l.Notify(e.Release)
				}
			}
			l.Notify("") // the stream broke: Docker may have restarted, look at everything
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
