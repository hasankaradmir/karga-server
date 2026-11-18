// Package release models installed packages, their immutable revisions and operations.
package release

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Status is the lifecycle state of a release.
type Status string

// Release statuses.
const (
	StatusPending      Status = "pending"
	StatusDeploying    Status = "deploying"
	StatusDeployed     Status = "deployed"
	StatusUpgrading    Status = "upgrading"
	StatusFailed       Status = "failed"
	StatusUninstalling Status = "uninstalling"
	StatusUninstalled  Status = "uninstalled"
)

var transitions = map[Status][]Status{
	StatusPending:      {StatusDeploying},
	StatusDeploying:    {StatusDeployed, StatusFailed},
	StatusDeployed:     {StatusUpgrading, StatusUninstalling},
	StatusUpgrading:    {StatusDeployed, StatusFailed},
	StatusFailed:       {StatusDeploying, StatusUpgrading, StatusUninstalling},
	StatusUninstalling: {StatusUninstalled, StatusFailed},
	StatusUninstalled:  {StatusDeploying},
}

// Transition validates moving from one status to another.
func Transition(from, to Status) error {
	if slices.Contains(transitions[from], to) {
		return nil
	}
	return fmt.Errorf("release cannot go from %s to %s", from, to)
}

// Release is an installed instance of a package.
type Release struct {
	Name           string
	PackageName    string
	PackageVersion string
	// CurrentRevision is the last successfully deployed revision; 0 if none.
	CurrentRevision int
	Status          Status
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Paused releases are left alone by the reconciler.
	Paused bool
}

// RevisionStatus is the outcome of one revision.
type RevisionStatus string

// Revision statuses.
const (
	RevisionPending  RevisionStatus = "pending"
	RevisionDeployed RevisionStatus = "deployed"
	RevisionFailed   RevisionStatus = "failed"
	// RevisionSuperseded is shown, never stored: a deployed revision a newer deployed one replaced.
	RevisionSuperseded RevisionStatus = "superseded"
)

// Revision is one immutable desired configuration of a release.
type Revision struct {
	Release        string
	Number         int
	PackageName    string
	PackageVersion string
	// RuntimeSupported is the package's runtime.supported, needed to re-check a rollback.
	RuntimeSupported []string
	PackageDigest    string
	ValuesDigest     string
	ManifestDigest   string
	// Values are the user overrides (-f, --set); ValuesAll is the merged result used to render.
	Values    map[string]any
	ValuesAll map[string]any
	// Manifest is the rendered resources as a JSON array, with secret values replaced by digests.
	Manifest json.RawMessage
	// Notes is the package's rendered NOTES.txt (writeOnly values redacted).
	Notes       string
	Status      RevisionStatus
	Description string
	CreatedAt   time.Time
}

// AppliedResource is what is currently applied on the runtime for one resource of a release.
type AppliedResource struct {
	Kind        string
	Name        string
	Revision    int
	RuntimeName string
	// Spec is the canonical redacted spec JSON; Digest covers it plus mounted content.
	Spec   json.RawMessage
	Digest string
}

// OperationStatus is the state of an operation.
type OperationStatus string

// Operation statuses.
const (
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationAborted   OperationStatus = "aborted"
)

// Operation types: what a user's command (install, upgrade, rollback, uninstall) or karga-server
// itself (reconcile, resume) does to a release.
const (
	OpInstall   = "install"
	OpUpgrade   = "upgrade"
	OpRollback  = "rollback"
	OpUninstall = "uninstall"
	OpReconcile = "reconcile"
	OpResume    = "resume"
)

// Operation records a mutating command so an interrupted one can be detected.
type Operation struct {
	ID       int64
	Release  string
	Type     string
	Revision int
	Status   OperationStatus
	PID      int
	Hostname string
	// Actor is who asked for it: a user of the karga CLI, or karga-server (repairs, resumes).
	Actor string
	// Options are the request's choices, so an interrupted operation resumes as it was asked.
	Options    Options
	Steps      []Step
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Options are the choices of the request that started an operation.
type Options struct {
	DeleteVolumes bool
	Strategy      string
	Timeout       time.Duration
	KeepHistory   bool
}

// Step is one runtime action of an operation.
type Step struct {
	Kind   string
	Name   string
	Action string
	Status string
	Error  string
}
