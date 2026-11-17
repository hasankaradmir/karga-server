//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/application/releases"
)

// TestDependsOnWaitsForTheService: api depends on Service web, whose server needs 3 seconds to
// listen; api's container is created only after web is ready behind the release proxy.
func TestDependsOnWaitsForTheService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	name := releaseName("it-dep")
	s, _ := service(t, name)
	if _, err := s.Install(ctx, releases.InstallRequest{CommonOptions: opts(), Name: name, Package: webPkg,
		Values: packages.Options{Sets: []string{"second=true", "apiDependsOn=true", "startDelay=3", "strategy=recreate"}}}); err != nil {
		t.Fatal(err)
	}
	created := func(c string) time.Time {
		t.Helper()
		out, err := exec.Command("docker", "inspect", "-f", "{{.Created}}", c).Output()
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	web, api := created(name+"-web"), created(name+"-api")
	if gap := api.Sub(web); gap < 3*time.Second {
		t.Fatalf("api was created %s after web, before web could be ready", gap)
	}
}
