// Package reconciler decides when karga-server reconciles a release: on Docker events, on a timer
// and when told to. What a reconciliation does is releases.Lifecycle.Repair.
package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
	// warned is the warning last logged per subject: one that repeats every pass (Docker down,
	// a repair given up on) is logged once, and its end once.
	warned map[string]string
}

// New returns a loop that takes releases from q, lists them with list and repairs them with
// repair, watching the runtime's container events.
func New(q *Queue, list Lister, repair Repairer, events EventSource, o Options) *Loop {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Loop{q: q, list: list, repair: repair, events: events, interval: o.Interval, log: o.Log, only: o.Only,
		retries: map[string]*time.Timer{}, warned: map[string]string{}}
}

// warn logs msg about subject at warn level, unless it is the warning last logged about it.
func (l *Loop) warn(subject, msg, why string, args ...any) {
	l.mu.Lock()
	again := l.warned[subject] == why
	l.warned[subject] = why
	l.mu.Unlock()
	if !again {
		l.log.Warn(msg, append(args, "error", why)...)
	}
}

// clear forgets subject's warning, logging msg if there was one (the problem is gone).
func (l *Loop) clear(subject, msg string, args ...any) {
	l.mu.Lock()
	_, was := l.warned[subject]
	delete(l.warned, subject)
	l.mu.Unlock()
	if was && msg != "" {
		l.log.Info(msg, args...)
	}
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
		l.warn("list", "reconcile: list releases", err.Error())
		return
	}
	l.clear("list", "reconcile: list releases works again")
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
		l.warn("release/"+name, "reconcile", err.Error(), "release", name)
		return
	case len(res.Actions) > 0:
		for _, a := range res.Actions {
			l.log.Info("reconciled", "release", name, "resource", a.Kind+"/"+a.Name, "action", a.Action, "status", a.Status, "error", a.Error)
		}
	}
	l.clear("release/"+name, "reconcile works again", "release", name)
	l.mu.Lock()
	for subject := range l.warned { // what karga no longer gives up on (it tries again, or got repaired)
		if what, ok := strings.CutPrefix(subject, "gaveup/"+name+"/"); ok && res.GaveUp[what] == "" {
			delete(l.warned, subject)
		}
	}
	l.mu.Unlock()
	for what, why := range res.GaveUp {
		l.warn("gaveup/"+name+"/"+what, "reconcile gave up", why, "release", name, "resource", what)
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
			l.warn("events", "reconcile: runtime events", err.Error())
		} else {
			began := time.Now()
			for e := range events {
				if e.Err == nil {
					l.clear("events", "reconcile: runtime events work again")
				} else if ctx.Err() == nil {
					if time.Since(began) > time.Minute {
						l.clear("events", "") // a stream that ran a while: a new outage, not the same one
					}
					l.warn("events", "reconcile: runtime events stopped", e.Err.Error())
				}
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
