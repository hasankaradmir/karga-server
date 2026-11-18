package deployment

import (
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// DefaultTimeout bounds how long Execute waits for workloads to become ready.
const DefaultTimeout = 5 * time.Minute

// Step statuses reported to ExecRequest.OnStep. StepKept means the change failed before anything
// running was touched: the previous state is still applied (a replace whose new container failed).
const (
	StepDone   = "done"
	StepFailed = "failed"
	StepKept   = "kept-previous"
)

// ExecError is returned by Execute. Disrupted is false when the failure happened before any running
// workload was stopped, removed or created: what ran before still runs, unchanged.
type ExecError struct {
	Err       error
	Disrupted bool
}

func (e *ExecError) Error() string { return e.Err.Error() }

func (e *ExecError) Unwrap() error { return e.Err }

// ExecRequest applies Plan, computed from Resources against the applied state, as revision Revision.
type ExecRequest struct {
	Release   string
	Revision  int
	Supported []string
	// Resources is the full desired manifest (pre-flight and materialization need all of it).
	Resources []resource.Resource
	Plan      Plan
	Timeout   time.Duration
	// OnStep is called after every runtime step so the caller can record applied state and progress.
	OnStep func(Change, release.Step) error
	// OnProgress, if set, receives what is going on as it happens (for display only).
	OnProgress func(Event)
	ProxyUser  string
	// OnWarning, if set, receives problems that do not stop the deployment.
	OnWarning func(string)
}
