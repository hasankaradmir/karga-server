package release

import (
	"errors"
	"reflect"
	"testing"
)

func TestInstall(t *testing.T) {
	for name, tc := range map[string]struct {
		existing *Release
		same     bool
		want     InstallAction
		err      error
	}{
		"no release":               {nil, false, InstallDeploy, nil},
		"deployed, same":           {&Release{Name: "r", Status: StatusDeployed, CurrentRevision: 2}, true, InstallNoOp, nil},
		"deployed, other content":  {&Release{Name: "r", Status: StatusDeployed, CurrentRevision: 2}, false, 0, ErrExists},
		"failed after deploying":   {&Release{Name: "r", Status: StatusFailed, CurrentRevision: 1}, false, 0, ErrExists},
		"failed, never deployed":   {&Release{Name: "r", Status: StatusFailed}, false, InstallDeploy, nil},
		"uninstalled":              {&Release{Name: "r", Status: StatusUninstalled, CurrentRevision: 3}, false, InstallDeploy, nil},
		"interrupted while deploy": {&Release{Name: "r", Status: StatusDeploying}, false, InstallDeploy, nil},
	} {
		got, err := Install(tc.existing, tc.same)
		if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
}

func TestCanUpgrade(t *testing.T) {
	if err := (Release{Name: "r", Status: StatusUninstalled}).CanUpgrade(); err == nil {
		t.Fatal("an uninstalled release was upgradable")
	}
	for _, st := range []Status{StatusDeployed, StatusFailed} {
		if err := (Release{Name: "r", Status: st}).CanUpgrade(); err != nil {
			t.Errorf("%s: %v", st, err)
		}
	}
}

func TestCanRollback(t *testing.T) {
	if err := (Release{Name: "r", Status: StatusUninstalled}).CanRollback(); err == nil {
		t.Fatal("an uninstalled release was rolled back")
	}
	if err := (Release{Name: "r", Status: StatusFailed}).CanRollback(); err != nil {
		t.Fatal(err)
	}
}

func TestPrunable(t *testing.T) {
	revs := make([]Revision, 8)
	for i := range revs {
		revs[i].Number = i + 1
	}
	// Keep the newest 3 (6, 7, 8), the current one (2) and one still applied (4).
	if got := Prunable(revs, 2, []int{4, 8}, 3); !reflect.DeepEqual(got, []int{1, 3, 5}) {
		t.Fatalf("prunable: %v", got)
	}
	if got := Prunable(revs, 8, nil, 0); got != nil {
		t.Fatalf("keep 0 keeps all: %v", got)
	}
	if got := Prunable(revs[:2], 2, nil, 5); got != nil {
		t.Fatalf("fewer than keep: %v", got)
	}
}

func TestInProgress(t *testing.T) {
	for st, want := range map[Status]bool{
		StatusPending: true, StatusDeploying: true, StatusUpgrading: true, StatusUninstalling: true,
		StatusDeployed: false, StatusFailed: false, StatusUninstalled: false, StatusUnreadable: false,
	} {
		if st.InProgress() != want {
			t.Errorf("%s: in progress %v", st, !want)
		}
	}
}

func TestClearInterrupted(t *testing.T) {
	for st, want := range map[Status]Status{
		StatusUpgrading: StatusFailed, StatusUninstalling: StatusFailed, StatusDeployed: StatusDeployed, StatusFailed: StatusFailed,
	} {
		r := Release{Status: st}
		if r.ClearInterrupted(); r.Status != want {
			t.Errorf("%s: %s, want %s", st, r.Status, want)
		}
	}
}

func TestStartDeploy(t *testing.T) {
	for name, tc := range map[string]struct {
		from    Status
		upgrade bool
		want    Status
		ok      bool
	}{
		"new":                 {StatusPending, false, StatusDeploying, true},
		"upgrade deployed":    {StatusDeployed, true, StatusUpgrading, true},
		"upgrade failed":      {StatusFailed, true, StatusUpgrading, true},
		"reinstall":           {StatusUninstalled, false, StatusDeploying, true},
		"install on deployed": {StatusDeployed, false, StatusDeployed, false},
	} {
		r := Release{Status: tc.from}
		previous, err := r.StartDeploy(tc.upgrade)
		if (err == nil) != tc.ok || r.Status != tc.want || (tc.ok && previous != tc.from) {
			t.Errorf("%s: %s, previous %s, %v", name, r.Status, previous, err)
		}
	}
}

func TestDeployFailed(t *testing.T) {
	r := Release{Status: StatusUpgrading}
	if r.DeployFailed(StatusDeployed, true); r.Status != StatusDeployed {
		t.Errorf("a failed deploy that changed nothing: %s, want deployed (D18)", r.Status)
	}
	r = Release{Status: StatusUpgrading}
	if r.DeployFailed(StatusDeployed, false); r.Status != StatusFailed {
		t.Errorf("a failed deploy that changed something: %s", r.Status)
	}
}

func TestDeployed(t *testing.T) {
	r := Release{Status: StatusUpgrading, CurrentRevision: 1, PackageName: "a", PackageVersion: "1"}
	r.Deployed(2, "b", "2")
	if r != (Release{Status: StatusDeployed, CurrentRevision: 2, PackageName: "b", PackageVersion: "2"}) {
		t.Fatalf("%+v", r)
	}
}

func TestRollbackTarget(t *testing.T) {
	revs := []Revision{{Number: 1, Status: RevisionDeployed}, {Number: 2, Status: RevisionFailed}, {Number: 3, Status: RevisionDeployed}, {Number: 4, Status: RevisionDeployed}}
	for name, tc := range map[string]struct {
		rel  Release
		want int
		ok   bool
	}{
		"failed: its current revision": {Release{Status: StatusFailed, CurrentRevision: 3}, 3, true},
		"deployed: the one before":     {Release{Status: StatusDeployed, CurrentRevision: 4}, 3, true},
		"skips failed revisions":       {Release{Status: StatusDeployed, CurrentRevision: 3}, 1, true},
		"none before":                  {Release{Name: "r", Status: StatusDeployed, CurrentRevision: 1}, 0, false},
		"failed, never deployed":       {Release{Name: "r", Status: StatusFailed}, 0, false},
	} {
		got, err := RollbackTarget(tc.rel, revs)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("%s: %d %v", name, got, err)
		}
	}
}

func TestUnreconcilable(t *testing.T) {
	for name, tc := range map[string]struct {
		rel  Release
		want string
	}{
		"deployed":       {Release{Status: StatusDeployed, CurrentRevision: 1}, ""},
		"paused":         {Release{Status: StatusDeployed, CurrentRevision: 1, Paused: true}, "paused"},
		"failed":         {Release{Status: StatusFailed, CurrentRevision: 1}, "the release is failed; karga reconciles deployed releases only"},
		"never deployed": {Release{Status: StatusDeployed}, "the release is deployed; karga reconciles deployed releases only"},
	} {
		if got := tc.rel.Unreconcilable(); got != tc.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
