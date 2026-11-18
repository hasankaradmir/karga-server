package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"
)

// Event reports what Execute is doing, for a live progress display.
type Event = api.Event

// Progress phases.
const (
	PhasePull   = api.PhasePull
	PhaseCreate = api.PhaseCreate
	PhaseStart  = api.PhaseStart
	PhaseWait   = api.PhaseWait
	PhaseSwitch = api.PhaseSwitch
	PhaseDrain  = api.PhaseDrain
	PhaseStop   = api.PhaseStop
	PhaseRemove = api.PhaseRemove
	PhaseDone   = api.PhaseDone
	PhaseFailed = api.PhaseFailed
)

type progressKey struct{}

func withProgress(ctx context.Context, fn func(Event)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// progress reports an event to the callback carried by ctx, if any.
func progress(ctx context.Context, kind, name, phase, format string, args ...any) {
	if fn, ok := ctx.Value(progressKey{}).(func(Event)); ok {
		fn(Event{Kind: kind, Name: name, Phase: phase, Detail: fmt.Sprintf(format, args...)})
	}
}

// waitEvery is how often a long wait reports that it is still waiting.
var waitEvery = 2 * time.Second
