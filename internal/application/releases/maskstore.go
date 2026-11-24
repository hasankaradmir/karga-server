package releases

import (
	"context"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// maskingStore is a state store that masks secret values in what operations record: their error
// and their steps' errors carry the messages of whatever failed, which may quote a secret.
type maskingStore struct {
	ports.StateStore
	mask func(string) string
}

func (m maskingStore) masked(op release.Operation) release.Operation {
	op.Error = m.mask(op.Error)
	op.Steps = m.steps(op.Steps)
	return op
}

func (m maskingStore) steps(steps []release.Step) []release.Step {
	out := make([]release.Step, len(steps))
	for i, s := range steps {
		s.Error = m.mask(s.Error)
		out[i] = s
	}
	return out
}

func (m maskingStore) BeginOperation(ctx context.Context, op release.Operation, onRunning func(release.Operation) error) (int64, error) {
	return m.StateStore.BeginOperation(ctx, m.masked(op), onRunning)
}

func (m maskingStore) FinishOperation(ctx context.Context, op release.Operation) error {
	return m.StateStore.FinishOperation(ctx, m.masked(op))
}

func (m maskingStore) SaveOperationSteps(ctx context.Context, rel string, id int64, steps []release.Step) error {
	return m.StateStore.SaveOperationSteps(ctx, rel, id, m.steps(steps))
}

func (m maskingStore) InTx(ctx context.Context, fn func(ports.StateStore) error) error {
	return m.StateStore.InTx(ctx, func(tx ports.StateStore) error { return fn(maskingStore{tx, m.mask}) })
}
