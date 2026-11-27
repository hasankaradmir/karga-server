package releases

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/hostnet"
	"github.com/hasankaradmir/karga-server/internal/ports/runtimetest"
)

const redisPkg = "../../../tests/testdata/redis"

type env struct {
	t    *testing.T
	svc  *Service
	rt   *fakeruntime.Runtime
	port string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	store, err := filestore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt := fakeruntime.New().WithGateway()
	host := &proxyops.Host{Proxies: fakeruntime.Proxies{}, Net: hostnet.Net{}}
	info, err := rt.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	host.UseRuntime(info)
	svc, err := New(Deps{Store: store, Runtime: rt, Host: host, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, rt: rt, port: strconv.Itoa(runtimetest.FreePort(t))}
}

func (e *env) opts(sets ...string) Values {
	return Values{Sets: packages.Sets(append([]string{"service.enabled=false"}, sets...)...)}
}

func common() CommonOptions {
	return CommonOptions{Timeout: time.Second}
}

func (e *env) install(sets ...string) (*Result, error) {
	return e.svc.Lifecycle.Install(context.Background(), InstallRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts(sets...)}, nil)
}

func (e *env) upgrade(sets ...string) (*Result, error) {
	return e.svc.Lifecycle.Upgrade(context.Background(), UpgradeRequest{CommonOptions: common(), Name: "cache", Package: redisPkg, Values: e.opts(sets...)}, nil)
}

func (e *env) release() release.Release {
	e.t.Helper()
	r, err := e.svc.c.store.GetRelease(context.Background(), "cache")
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) image() string { return e.rt.Workload("cache-redis").Request.Image }

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

const webPkg = "../../../tests/integration/testdata/web"

const pgPkg = "../../../tests/testdata/postgres"

// scratchPkg writes a package whose volume "scratch" (retention delete) exists while .Values.scratch is true.
func scratchPkg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Package.yaml": "apiVersion: karga.io/v1alpha1\nname: scratch\nversion: 0.1.0\nruntime:\n  supported: [docker]\n",
		"values.yaml":  "scratch: true\n",
		"templates/r.yaml": `{{- if .Values.scratch }}
apiVersion: karga.io/v1alpha1
kind: Volume
metadata: {name: scratch}
spec: {retention: delete}
---
{{- end }}
apiVersion: karga.io/v1alpha1
kind: Workload
metadata: {name: app}
spec:
  image: busybox:1.37
  command: [sleep, "300"]
`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// changed reports whether a plan touches the runtime.
func changed(plan []deployment.Summary) bool {
	for _, c := range plan {
		if c.Action != deployment.ActionUnchanged {
			return true
		}
	}
	return false
}
