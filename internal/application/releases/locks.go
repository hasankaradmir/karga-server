package releases

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// opLocks serialize the operations of each release inside this process: a user's command waits
// for a reconciliation in progress instead of failing; a reconciliation skips a busy release.
type opLocks struct {
	mu   sync.Mutex
	busy map[string]*releaseLock // releases someone holds or waits for
}

// lock takes release rel for one operation, from reading its state to recording the outcome, so
// what an operation plans cannot go stale before it runs. It waits until rel is free or the wait's
// context (WithWait; ctx otherwise) ends. Call the returned function to let go.
func (o *opLocks) lock(ctx context.Context, rel string) (func(), error) {
	l := o.ref(rel)
	wait := waitContext(ctx)
	select {
	case l.ch <- struct{}{}:
		return o.unlock(rel, l), nil
	case <-wait.Done():
		o.unref(rel)
		return nil, fmt.Errorf("waiting for the operation running on %s: %w", rel, wait.Err())
	}
}

type waitKey struct{}

// WithWait bounds by wait what an operation on ctx waits for before it changes anything (a busy
// release's lock), without canceling the operation: a command whose client went away stops
// waiting, one that started goes on (ADR-0015).
func WithWait(ctx, wait context.Context) context.Context {
	return context.WithValue(ctx, waitKey{}, wait)
}

func waitContext(ctx context.Context) context.Context {
	if w, ok := ctx.Value(waitKey{}).(context.Context); ok {
		return w
	}
	return ctx
}

// tryLock is lock without waiting: it fails with ErrLocked when rel is busy.
func (o *opLocks) tryLock(rel string) (func(), error) {
	l := o.ref(rel)
	select {
	case l.ch <- struct{}{}:
		return o.unlock(rel, l), nil
	default:
		o.unref(rel)
		return nil, fmt.Errorf("%w: a command is working on %s", ErrLocked, rel)
	}
}

func (o *opLocks) unlock(rel string, l *releaseLock) func() {
	return func() {
		<-l.ch
		o.unref(rel)
	}
}

// releaseLock is held while its channel (capacity 1) is full; refs counts who holds or waits for
// it, so a release nobody uses has no entry.
type releaseLock struct {
	ch   chan struct{}
	refs int
}

func (o *opLocks) ref(rel string) *releaseLock {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.busy == nil {
		o.busy = map[string]*releaseLock{}
	}
	l := o.busy[rel]
	if l == nil {
		l = &releaseLock{ch: make(chan struct{}, 1)}
		o.busy[rel] = l
	}
	l.refs++
	return l
}

func (o *opLocks) unref(rel string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if l := o.busy[rel]; l != nil {
		if l.refs--; l.refs == 0 {
			delete(o.busy, rel)
		}
	}
}

func describe(op release.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s of revision %d started %s by pid %d on %s", op.Type, op.Revision, op.StartedAt.Local().Format(time.RFC3339), op.PID, op.Hostname)
	for _, s := range op.Steps {
		fmt.Fprintf(&b, "\n  %s %s %s: %s", s.Action, s.Kind, s.Name, s.Status)
		if s.Error != "" {
			fmt.Fprintf(&b, " (%s)", s.Error)
		}
	}
	return b.String()
}

// begin records a running operation; the caller holds the release's lock. An interrupted operation
// is only overridden with force (D15).
func (s *core) begin(ctx context.Context, rel, typ string, revision int, force bool, opts release.Options) (release.Operation, error) {
	s.recordUnfinished(ctx, rel)
	op := release.Operation{Release: rel, Type: typ, Revision: revision, Status: release.OperationRunning,
		PID: s.pid, Hostname: s.hostname, Actor: actorOf(ctx), Options: opts, StartedAt: s.now()}
	id, err := s.store.BeginOperation(ctx, op, func(running release.Operation) error {
		// Under the release's lock no operation of this process runs, and only one karga-server runs
		// on a home (its lock file): an operation left running is an earlier process's, whatever
		// process has its PID now.
		if !force {
			return fmt.Errorf("%w: %s\nInspect with 'karga status %s'; rerun with --force to continue", ErrInterrupted, describe(running), rel)
		}
		s.log.Warn("aborting interrupted operation", "release", rel, "operation", running.ID)
		return nil
	})
	op.ID = id
	if err == nil {
		s.log.Info("operation", "operation_id", id, "release", rel, "type", typ, "revision", revision, "user", op.Actor)
	}
	return op, err
}

type actorKey struct{}

// WithActor tells the operations started with ctx who asked for them.
func WithActor(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, actorKey{}, user)
}

// ServerActor is the actor of what karga-server does on its own: repairs and resumed operations.
const ServerActor = "karga-server"

func actorOf(ctx context.Context) string {
	if u, _ := ctx.Value(actorKey{}).(string); u != "" {
		return u
	}
	return ServerActor
}

// finish records the end of an operation. It uses a context that survives cancellation (Ctrl-C) so
// the operation is not left running.
func (s *core) finish(ctx context.Context, op release.Operation, err error) {
	ctx = context.WithoutCancel(ctx)
	op.Status = release.OperationSucceeded
	if err != nil {
		op.Status, op.Error = release.OperationFailed, err.Error()
	}
	attrs := []any{"operation_id", op.ID, "release", op.Release, "type", op.Type, "revision", op.Revision, "status", op.Status,
		"duration", s.now().Sub(op.StartedAt).Round(time.Millisecond).String()} // "10.8s" in JSON too
	if err != nil {
		s.log.Warn("operation finished", append(attrs, "error", err)...)
	} else {
		s.log.Info("operation finished", attrs...)
	}
	if ferr := s.store.FinishOperation(ctx, op); ferr != nil {
		s.log.Error("record operation result", "release", op.Release, "error", ferr)
		s.unrecorded.Store(op.Release, op) // not left running: written before the release's next operation
	}
}

// recordUnfinished writes the end of rel's operation finish could not, if any: once there is room again, the
// release is not taken for interrupted.
func (s *core) recordUnfinished(ctx context.Context, rel string) {
	v, ok := s.unrecorded.Load(rel)
	if !ok {
		return
	}
	if err := s.store.FinishOperation(context.WithoutCancel(ctx), v.(release.Operation)); err != nil {
		return // still no room: the operation that follows says so
	}
	s.unrecorded.Delete(rel) // the release's lock is held: no other end of it is stored meanwhile
}
