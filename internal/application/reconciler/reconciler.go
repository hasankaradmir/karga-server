// Package reconciler decides when karga-server reconciles a release: on Docker events, on a timer
// and when told to. What a reconciliation does is releases.Lifecycle.Repair.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Lister lists the releases to reconcile.
type Lister interface {
	List(ctx context.Context) ([]release.Release, error)
}

// Repairer repairs a release and says when a repair waiting out its backoff may run.
type Repairer interface {
	Repair(ctx context.Context, name string, force bool) (*releases.ReconcileResult, error)
	NextRepair(name string) time.Time
}

// EventSource streams what happens to karga's containers (a ports.Runtime).
type EventSource interface {
	Events(ctx context.Context) (<-chan ports.RuntimeEvent, error)
}

// Queue collects the releases to reconcile, after a debounce: a recreate fires several events.
// It is made before the release service, whose Deps.Notify is Queue.Notify, and the loop. A release
// waits in it once, however often it is notified, and none is ever dropped.
type Queue struct {
	debounce time.Duration
	mu       sync.Mutex
	pending  map[string]*time.Timer // debouncing
	ready    map[string]bool        // to reconcile; "" means every release
	wake     chan struct{}          // signalled when ready gets a name
}

// NewQueue returns a queue that delays a release's reconciliation by debounce (0: 2s).
func NewQueue(debounce time.Duration) *Queue {
	if debounce <= 0 {
		debounce = 2 * time.Second
	}
	return &Queue{debounce: debounce, pending: map[string]*time.Timer{}, ready: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// Notify asks for a reconciliation of release name after the debounce ("" means every release).
func (q *Queue) Notify(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.pending[name]; ok {
		t.Reset(q.debounce)
		return
	}
	var t *time.Timer
	t = time.AfterFunc(q.debounce, func() {
		q.mu.Lock()
		// A Notify that held the lock as t fired reset it, so t fires again: its first run took
		// the name already.
		if q.pending[name] != t {
			q.mu.Unlock()
			return
		}
		delete(q.pending, name)
		q.ready[name] = true
		q.mu.Unlock()
		select {
		case q.wake <- struct{}{}:
		default: // already signalled: the loop takes every ready name at once
		}
	})
	q.pending[name] = t
}

// take returns the releases ready to reconcile, sorted, and empties the set.
func (q *Queue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	names := slices.Sorted(maps.Keys(q.ready))
	clear(q.ready)
	return names
}

// Options configure a Loop.
type Options struct {
	// Interval between full passes over every release; 0 reacts to events only.
	Interval time.Duration
	Log      *slog.Logger
	// Only, if set, limits the loop to the releases it accepts (tests sharing a host).
	Only func(release string) bool
}

// Loop reconciles releases one at a time, so it costs nothing while all is well.
type Loop struct {
	q        *Queue
	list     Lister
	repair   Repairer
	events   EventSource
	interval time.Duration
	log      *slog.Logger
	only     func(string) bool

	mu      sync.Mutex
	retries map[string]*time.Timer // repairs waiting out their backoff
}

// New returns a loop that takes releases from q, lists them with list and repairs them with
// repair, watching the runtime's container events.
func New(q *Queue, list Lister, repair Repairer, events EventSource, o Options) *Loop {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Loop{q: q, list: list, repair: repair, events: events, interval: o.Interval, log: o.Log, only: o.Only, retries: map[string]*time.Timer{}}
}

// Run reconciles until ctx is done: every release at start (after a reboot, say), after events
// of a release's containers, and every Interval.
func (l *Loop) Run(ctx context.Context) {
	// Run owns what it starts: the event watch ends and the retry timers stop before it returns.
	var wg sync.WaitGroup
	wg.Go(func() { l.watch(ctx) })
	defer func() {
		l.mu.Lock()
		for name, t := range l.retries {
			t.Stop()
			delete(l.retries, name)
		}
		l.mu.Unlock()
		wg.Wait()
	}()
	var tick <-chan time.Time
	if l.interval > 0 {
		t := time.NewTicker(l.interval)
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
		case <-l.q.wake:
			names := l.q.take()
			if slices.Contains(names, "") {
				l.all(ctx)
				continue
			}
			for _, name := range names {
				if ctx.Err() != nil {
					return
				}
				l.one(ctx, name)
			}
		}
	}
}

func (l *Loop) all(ctx context.Context) {
	rels, err := l.list.List(ctx)
	if err != nil {
		l.log.Warn("reconcile: list releases", "error", err)
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
	if l.only != nil && !l.only(name) {
		return
	}
	// A repair that started finishes even when the server stops.
	res, err := l.repair.Repair(context.WithoutCancel(ctx), name, false)
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return // a container of another KARGA_HOME, or a release uninstalled meanwhile
	case err != nil:
		l.log.Warn("reconcile", "release", name, "error", err)
		return
	case len(res.Actions) > 0:
		for _, a := range res.Actions {
			l.log.Info("reconciled", "release", name, "resource", a.Kind+"/"+a.Name, "action", a.Action, "status", a.Status, "error", a.Error)
		}
	}
	for what, why := range res.GaveUp {
		l.log.Warn("reconcile gave up", "release", name, "resource", what, "error", why)
	}
	// A repair waiting out its backoff is retried when it may run, events or not.
	if at := l.repair.NextRepair(name); !at.IsZero() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.retries[name]; !ok {
			l.retries[name] = time.AfterFunc(time.Until(at), func() {
				l.mu.Lock()
				delete(l.retries, name)
				l.mu.Unlock()
				l.q.Notify(name)
			})
		}
	}
}

// watch turns the runtime's container events into reconciliations, reconnecting when the stream
// breaks (Docker restarted).
func (l *Loop) watch(ctx context.Context) {
	for ctx.Err() == nil {
		events, err := l.events.Events(ctx)
		if err != nil {
			l.log.Warn("reconcile: runtime events", "error", err)
		} else {
			for e := range events {
				if e.Release != "" && e.Action != "start" {
					l.q.Notify(e.Release)
				}
			}
			l.q.Notify("") // the stream broke: Docker may have restarted, look at everything
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
