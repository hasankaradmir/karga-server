package releases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// lock takes release rel for one operation, from reading its state to recording the outcome, so
// what an operation plans cannot go stale before it runs. It waits until rel is free or ctx ends;
// with try it does not wait. Call the returned function to let go.
func (s *Core) lock(ctx context.Context, rel string, try bool) (func(), error) {
	l := s.lockRef(rel)
	unlock := func() {
		<-l.ch
		s.lockUnref(rel)
	}
	select {
	case l.ch <- struct{}{}:
		return unlock, nil
	default:
	}
	if try {
		s.lockUnref(rel)
		return nil, fmt.Errorf("%w: a command is working on %s", ErrLocked, rel)
	}
	select {
	case l.ch <- struct{}{}:
		return unlock, nil
	case <-ctx.Done():
		s.lockUnref(rel)
		return nil, fmt.Errorf("waiting for the operation running on %s: %w", rel, ctx.Err())
	}
}

// releaseLock is held while its channel (capacity 1) is full; refs counts who holds or waits for
// it, so a release nobody uses has no entry.
type releaseLock struct {
	ch   chan struct{}
	refs int
}

func (s *Core) lockRef(rel string) *releaseLock {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busy == nil {
		s.busy = map[string]*releaseLock{}
	}
	l := s.busy[rel]
	if l == nil {
		l = &releaseLock{ch: make(chan struct{}, 1)}
		s.busy[rel] = l
	}
	l.refs++
	return l
}

func (s *Core) lockUnref(rel string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if l := s.busy[rel]; l != nil {
		if l.refs--; l.refs == 0 {
			delete(s.busy, rel)
		}
	}
}

// processAlive reports whether pid runs on this host (signal 0 only checks existence).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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

// begin records a running operation; the caller holds the release's lock. A live process holding the
// release always wins; a dead one is only overridden with force (D15).
func (s *Core) begin(ctx context.Context, rel, typ string, revision int, force bool, opts release.Options) (release.Operation, error) {
	op := release.Operation{Release: rel, Type: typ, Revision: revision, Status: release.OperationRunning,
		PID: s.PID, Hostname: s.Hostname, Actor: actorOf(ctx), Options: opts, StartedAt: s.Now()}
	id, err := s.Store.BeginOperation(ctx, op, func(running release.Operation) error {
		// Under the release mutex no operation of this process runs: one with its PID is from an
		// earlier process that got the same PID (a container's PID 1).
		if running.Hostname == s.Hostname && running.PID != s.PID && processAlive(running.PID) {
			return fmt.Errorf("%w: %s", ErrLocked, describe(running))
		}
		if !force {
			return fmt.Errorf("%w: %s\nInspect with 'karga status %s'; rerun with --force to continue", ErrInterrupted, describe(running), rel)
		}
		s.Log.Warn("aborting interrupted operation", "release", rel, "operation", running.ID)
		return nil
	})
	op.ID = id
	if err == nil {
		s.Log.Info("operation", "operation_id", id, "release", rel, "type", typ, "revision", revision, "user", op.Actor)
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
func (s *Core) finish(ctx context.Context, op release.Operation, err error) {
	ctx = context.WithoutCancel(ctx)
	op.Status = release.OperationSucceeded
	if err != nil {
		op.Status, op.Error = release.OperationFailed, err.Error()
	}
	if ferr := s.Store.FinishOperation(ctx, op); ferr != nil {
		s.Log.Error("record operation result", "release", op.Release, "error", ferr)
	}
}
