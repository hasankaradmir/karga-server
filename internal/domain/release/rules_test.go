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
