package release

import "testing"

func TestTransition(t *testing.T) {
	allowed := [][2]Status{
		{StatusPending, StatusDeploying},
		{StatusDeploying, StatusDeployed}, {StatusDeploying, StatusFailed},
		{StatusDeployed, StatusUpgrading}, {StatusDeployed, StatusUninstalling},
		{StatusUpgrading, StatusDeployed}, {StatusUpgrading, StatusFailed},
		{StatusFailed, StatusUpgrading}, {StatusFailed, StatusDeploying}, {StatusFailed, StatusUninstalling},
		{StatusUninstalling, StatusUninstalled},
		{StatusUninstalled, StatusDeploying},
	}
	for _, tr := range allowed {
		if err := Transition(tr[0], tr[1]); err != nil {
			t.Errorf("%s -> %s: %v", tr[0], tr[1], err)
		}
	}
	denied := [][2]Status{
		{StatusPending, StatusDeployed},
		{StatusDeployed, StatusDeployed},
		{StatusUninstalled, StatusUpgrading},
		{StatusDeploying, StatusUninstalling},
		{"", StatusDeployed},
	}
	for _, tr := range denied {
		if err := Transition(tr[0], tr[1]); err == nil {
			t.Errorf("%s -> %s: expected error", tr[0], tr[1])
		}
	}
}
