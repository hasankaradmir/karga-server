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
