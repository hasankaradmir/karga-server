package ports

import (
	"context"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// StateStore persists releases, revisions, applied resources and operations.
// Getters return an error wrapping ErrNotFound for missing rows.
type StateStore interface {
	// InTx runs fn in one transaction: the writes fn makes through the store it is given all
	// happen, or none do (fn returned an error). BeginOperation cannot run inside it.
	InTx(ctx context.Context, fn func(StateStore) error) error

	GetRelease(ctx context.Context, name string) (release.Release, error)
	ListReleases(ctx context.Context) ([]release.Release, error)
	// SaveRelease inserts or updates a release.
	SaveRelease(ctx context.Context, r release.Release) error
	// DeleteRelease removes a release with its revisions and resources.
	DeleteRelease(ctx context.Context, name string) error

	CreateRevision(ctx context.Context, rev release.Revision) error
	// UpdateRevisionStatus changes only status and description: revisions are otherwise immutable.
	UpdateRevisionStatus(ctx context.Context, rel string, number int, status release.RevisionStatus, description string) error
	GetRevision(ctx context.Context, rel string, number int) (release.Revision, error)
	// ListRevisions returns revisions in ascending order.
	ListRevisions(ctx context.Context, rel string) ([]release.Revision, error)
	DeleteRevision(ctx context.Context, rel string, number int) error

	ListResources(ctx context.Context, rel string) ([]release.AppliedResource, error)
	// SaveResource inserts or replaces the applied state of one resource.
	SaveResource(ctx context.Context, rel string, r release.AppliedResource) error
	DeleteResource(ctx context.Context, rel, kind, name string) error

	// BeginOperation atomically records op as running. If another operation of the same release is
	// still running, onRunning decides: an error aborts BeginOperation, nil marks that operation aborted.
	BeginOperation(ctx context.Context, op release.Operation, onRunning func(release.Operation) error) (int64, error)
	// FinishOperation stores the final status, steps and error of an operation.
	FinishOperation(ctx context.Context, op release.Operation) error
	// SaveOperationSteps records progress so an interrupted operation shows how far it got.
	SaveOperationSteps(ctx context.Context, rel string, id int64, steps []release.Step) error
	// RunningOperation returns the running operation of a release, or ErrNotFound.
	RunningOperation(ctx context.Context, rel string) (release.Operation, error)
	// ListOperations returns the operations of a release (all releases for ""), newest first: at most
	// limit, and only those with an ID above afterID.
	ListOperations(ctx context.Context, rel string, afterID int64, limit int) ([]release.Operation, error)
}
