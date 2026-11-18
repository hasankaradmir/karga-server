package releases

import (
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// What the release service is asked and what it answers. internal/server maps them to and from the
// API (karga-sdk/api); the service knows nothing of the wire.

// CommonOptions apply to every mutating command. Zero values leave the choice to the service.
type CommonOptions struct {
	DryRun bool
	// Timeout bounds how long the operation waits for workloads to become ready (0: DefaultTimeout).
	Timeout time.Duration
	// HistoryMax is how many revisions the release keeps (nil: DefaultHistoryMax; 0: unlimited).
	HistoryMax *int
	// Force aborts an interrupted operation (whose process is gone) instead of refusing to run.
	Force bool
	// DeleteVolumes allows a plan to delete volumes with retention delete, and their data.
	DeleteVolumes bool
}

// Values are what a user gives an install or upgrade, applied in this order onto the defaults.
type Values struct {
	Files          [][]byte // contents of values files
	Set, SetString []string // --set and --set-string expressions
	Strict         bool     // reject values the schema does not declare
}

// InstallRequest installs the package at Package (a directory or .tgz on this host).
type InstallRequest struct {
	CommonOptions
	Name, Package string
	Values        Values
}

// UpgradeRequest moves a release to a new package version or new values.
type UpgradeRequest struct {
	CommonOptions
	Name, Package string
	Values        Values
	// ReuseValues starts from the current revision's overrides.
	ReuseValues bool
	// Install installs the release if it does not exist.
	Install bool
	// Strategy overrides the workloads' updateStrategy (recreate or replace); empty uses the spec.
	Strategy string
}

// RollbackRequest re-applies an earlier revision as a new revision.
type RollbackRequest struct {
	CommonOptions
	Name string
	// Revision 0 selects the last deployed revision before the current state.
	Revision int
	Strategy string
}

// UninstallRequest removes a release's runtime objects.
type UninstallRequest struct {
	Name                                      string
	Force, DeleteVolumes, KeepHistory, DryRun bool
}

// Result describes what a mutating command did.
type Result struct {
	Release        string
	Revision, From int
	Status         string
	DryRun         bool
	// NoOp is set when an install found the release already deployed with the same package and values.
	NoOp bool
	Plan []deployment.Summary
	// Warnings are problems that did not stop the operation.
	Warnings []string
	// Notes is the package's NOTES.txt for this revision.
	Notes string
}

// LogsRequest selects containers to show logs of: the workloads of a release (all, or the named
// ones), its proxy, or the host gateway.
type LogsRequest struct {
	Release        string
	Workloads      []string
	Proxy, Gateway bool
	// Tail is the number of lines per container from the end (negative: all).
	Tail   int
	Follow bool
}

// VolumeBackupRequest copies a volume's files out, as a gzip-compressed tar. Stop stops the
// workloads mounting it for a consistent copy; otherwise the copy is taken live.
type VolumeBackupRequest struct {
	Release, Volume string
	Stop            bool
}

// VolumeRestoreRequest writes a backup into a volume. Force restores a backup of another volume.
type VolumeRestoreRequest struct {
	Release, Volume string
	Force           bool
	// Name names the backup in messages (its file name).
	Name string
}

// ResourceStatus compares one applied resource with what the runtime reports.
type ResourceStatus struct {
	Kind, Name, RuntimeName string
	Revision                int
	// Desired is "applied", or "failed" when the last attempt to apply it failed.
	Desired string
	// Actual is the runtime state: running/healthy, exited, present or missing.
	Actual, Health string
	// Backend is the alias a proxy routes to.
	Backend string
	// Restarts counts restarts by the runtime; OOMKilled says the last run hit the memory limit.
	Restarts  int
	OOMKilled bool
}

// StatusReport is the desired vs actual state of a release.
type StatusReport struct {
	Release   release.Release
	Resources []ResourceStatus
	// Orphans are runtime objects labelled with the release that karga does not track.
	Orphans []string
	// Interrupted describes an operation that is still marked running.
	Interrupted string
	// Reconcile is the last reconciliation and what karga gave up repairing.
	Reconcile *ReconcileStatus
}

// ReconcileStatus is the reconciliation part of a status report.
type ReconcileStatus struct {
	Last          time.Time
	Status, Error string
	GaveUp        map[string]string
}

// RevisionDiff is how two revisions of a release differ.
type RevisionDiff struct {
	Release         string
	From, To        int
	Package, Values []deployment.FieldChange
	Resources       []deployment.ResourceDiff
}

// ReconcileResult is what one reconciliation of a release did.
type ReconcileResult struct {
	Release string
	// Skipped says why nothing was looked at.
	Skipped string
	Actions []release.Step
	// GaveUp lists the resources karga stopped repairing, with their last error.
	GaveUp map[string]string
}
