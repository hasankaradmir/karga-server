package release

import (
	"errors"
	"fmt"
	"slices"
)

// ErrExists is returned when a release already runs something else than what is being installed.
var ErrExists = errors.New("release already exists")

// InstallAction is what an install of a package does to a release.
type InstallAction int

// Install actions.
const (
	// InstallDeploy deploys a new revision: there is no release, or the one there never deployed,
	// was uninstalled or was interrupted.
	InstallDeploy InstallAction = iota
	// InstallNoOp changes nothing: the release is deployed with the same package and values.
	InstallNoOp
)

// Install decides what an install does to existing (nil if there is no release). same reports
// whether existing's current revision has the package and values being installed. A deployed
// release with other content, and one that failed after it had deployed, are upgraded or rolled
// back instead.
func Install(existing *Release, same bool) (InstallAction, error) {
	switch {
	case existing == nil:
		return InstallDeploy, nil
	case existing.Status == StatusDeployed && same:
		return InstallNoOp, nil
	case existing.Status == StatusDeployed:
		return 0, fmt.Errorf("%w: %s is deployed with a different package or values; use 'karga upgrade'", ErrExists, existing.Name)
	case existing.Status == StatusFailed && existing.CurrentRevision > 0:
		return 0, fmt.Errorf("%w: %s failed after revision %d; use 'karga upgrade' or 'karga rollback'", ErrExists, existing.Name, existing.CurrentRevision)
	}
	return InstallDeploy, nil
}

// CanUpgrade checks that r can be upgraded: an uninstalled release is installed again instead.
func (r Release) CanUpgrade() error {
	if r.Status == StatusUninstalled {
		return fmt.Errorf("release %s is uninstalled; use 'karga install'", r.Name)
	}
	return nil
}

// CanRollback checks that r can be rolled back: an uninstalled release has nothing running to
// roll back and is installed again instead (its history stays for that).
func (r Release) CanRollback() error {
	if r.Status == StatusUninstalled {
		return fmt.Errorf("release %s is uninstalled; use 'karga install' (its history is kept)", r.Name)
	}
	return nil
}

// Prunable returns the revisions (numbers, ascending) to delete so that at most keep remain, never
// current nor one whose resources are still applied. revs are the release's revisions, ascending;
// keep 0 keeps them all.
func Prunable(revs []Revision, current int, applied []int, keep int) []int {
	if keep <= 0 {
		return nil
	}
	var out []int
	for _, r := range revs[:max(0, len(revs)-keep)] {
		if r.Number != current && !slices.Contains(applied, r.Number) {
			out = append(out, r.Number)
		}
	}
	return out
}

// InProgress reports whether s is the status of an operation that has not ended. A stored release in
// such a status was left behind by an interrupted operation.
func (s Status) InProgress() bool {
	switch s {
	case StatusPending, StatusDeploying, StatusUpgrading, StatusUninstalling:
		return true
	}
	return false
}

// ClearInterrupted marks r failed when an interrupted operation left it in progress: the operation
// that begins next has just aborted that one.
func (r *Release) ClearInterrupted() {
	if r.Status.InProgress() {
		r.Status = StatusFailed
	}
}

// StartDeploy moves r to deploying, or upgrading when upgrade, and returns the status it had.
func (r *Release) StartDeploy(upgrade bool) (Status, error) {
	target := StatusDeploying
	if upgrade {
		target = StatusUpgrading
	}
	if err := Transition(r.Status, target); err != nil {
		return "", err
	}
	previous := r.Status
	r.Status = target
	return previous, nil
}

// DeployFailed records a failed deploy of r. One that changed nothing on an existing release leaves
// it with the status it had before (previous) (D18); otherwise r failed.
func (r *Release) DeployFailed(previous Status, changedNothing bool) {
	r.Status = StatusFailed
	if changedNothing {
		r.Status = previous
	}
}

// Deployed records that revision of package name and version runs now.
func (r *Release) Deployed(revision int, name, version string) {
	r.Status, r.CurrentRevision = StatusDeployed, revision
	r.PackageName, r.PackageVersion = name, version
}

// RollbackTarget picks the revision to return r to: after a failure the last good one (the current
// revision), otherwise the last deployed revision before the current one. revs are r's revisions,
// ascending.
func RollbackTarget(r Release, revs []Revision) (int, error) {
	if r.Status == StatusFailed && r.CurrentRevision > 0 {
		return r.CurrentRevision, nil
	}
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].Number < r.CurrentRevision && revs[i].Status == RevisionDeployed {
			return revs[i].Number, nil
		}
	}
	return 0, fmt.Errorf("release %s has no earlier deployed revision to roll back to", r.Name)
}

// Unreconcilable says why the reconciler leaves r alone ("" if it does not): paused, or not deployed.
func (r Release) Unreconcilable() string {
	switch {
	case r.Paused:
		return "paused"
	case r.Status != StatusDeployed || r.CurrentRevision == 0:
		return fmt.Sprintf("the release is %s; karga reconciles deployed releases only", r.Status)
	}
	return ""
}
