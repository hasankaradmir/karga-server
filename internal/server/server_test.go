package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
	"github.com/hasankaradmir/karga-server/internal/application/releases"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

const redisPkg = "../../tests/testdata/redis"

// start runs a karga-server on home with the fake runtime until the test ends and returns a client.
func start(t *testing.T, home string) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "ksrv") // unix socket paths are short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "karga.sock")
	s, err := New(context.Background(), Config{Home: home, Socket: sock, ReconcileInterval: -1}, fakeruntime.New().WithGateway())
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// The grace outlasts net/http's 5 s for a connection that never sent a request: the client's
	// transport may dial one it then does not use, and Shutdown waits for it.
	go func() { done <- s.Serve(ctx, l, 10*time.Second) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		_ = s.Close()
	})
	return api.NewClient(sock)
}

func withValues(sets ...string) api.ValuesInput {
	in := api.ValuesInput{Sets: []api.Set{{Expr: "service.enabled=false"}}}
	for _, s := range sets {
		in.Sets = append(in.Sets, api.Set{Expr: s})
	}
	return in
}

// The API reaches the release service through the composition root, and the service's errors
// reach the client with their kind.
func TestServeThroughTheAPI(t *testing.T) {
	c := start(t, t.TempDir())
	ctx := context.Background()
	opts := api.CommonOptions{Timeout: api.Duration(time.Second)}

	res, err := c.Install(ctx, api.InstallRequest{CommonOptions: opts, Name: "cache", Package: redisPkg, Values: withValues()}, nil)
	if err != nil || res.Status != api.StatusDeployed || res.Revision != 1 {
		t.Fatalf("install: %+v %v", res, err)
	}
	rels, err := c.List(ctx)
	if err != nil || len(rels) != 1 || rels[0].Name != "cache" || rels[0].Status != api.StatusDeployed {
		t.Fatalf("list: %+v %v", rels, err)
	}
	rep, err := c.Status(ctx, api.ReleaseRequest{Name: "cache"})
	if err != nil || len(rep.Resources) == 0 {
		t.Fatalf("status: %+v %v", rep, err)
	}

	for _, tc := range []struct {
		name string
		call func() error
		want error
	}{
		{"status of a missing release", func() error { _, err := c.Status(ctx, api.ReleaseRequest{Name: "nope"}); return err }, api.ErrNotFound},
		{"install over a different deployed release", func() error {
			_, err := c.Install(ctx, api.InstallRequest{CommonOptions: opts, Name: "cache", Package: redisPkg, Values: withValues("image.tag=7.2")}, nil)
			return err
		}, api.ErrConflict},
		{"values the schema refuses", func() error {
			_, err := c.Install(ctx, api.InstallRequest{CommonOptions: opts, Name: "other", Package: redisPkg, Values: withValues("service.port=nope")}, nil)
			return err
		}, api.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// Only one karga-server may use a KARGA_HOME.
func TestOneServerPerHome(t *testing.T) {
	home := t.TempDir()
	start(t, home)
	if _, err := New(context.Background(), Config{Home: home, Socket: filepath.Join(t.TempDir(), "s"), ReconcileInterval: -1}, fakeruntime.New()); err == nil {
		t.Fatal("a second server on the same home was not refused")
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want error
	}{
		{fmt.Errorf("x: %w", ports.ErrNotFound), api.ErrNotFound},
		{fmt.Errorf("x: %w", ports.ErrInvalid), api.ErrInvalid},
		{releases.ErrLocked, api.ErrConflict},
		{releases.ErrInterrupted, api.ErrConflict},
		{releases.ErrExists, api.ErrConflict},
		{ports.ErrPortInUse, api.ErrConflict},
		{ports.ErrConflict, api.ErrConflict},
		{errors.New("anything else"), nil},
	} {
		if got := classify(tc.err); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("classify(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A log stream left open does not hold the server's shutdown, which waits for operations only
// (SI-03).
func TestShutdownEndsLogStreams(t *testing.T) {
	home := t.TempDir()
	dir, err := os.MkdirTemp("", "ksrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "karga.sock")
	s, err := New(context.Background(), Config{Home: home, Socket: sock, ReconcileInterval: -1}, fakeruntime.New().WithGateway())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	l, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l, time.Minute) }()
	c := api.NewClient(sock)
	if _, err := c.Install(context.Background(), api.InstallRequest{CommonOptions: api.CommonOptions{Timeout: api.Duration(time.Second)},
		Name: "cache", Package: redisPkg, Values: withValues()}, nil); err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Logs(context.Background(), api.LogsRequest{Release: "cache", Follow: true}, io.Discard) }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil || time.Since(start) > 10*time.Second {
			t.Fatalf("serve: %v after %s", err, time.Since(start))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the shutdown waited for the log stream")
	}
}

// What an earlier process left in tmp is removed at start.
func TestStartRemovesLeftovers(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, "tmp", "karga-package-1")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	start(t, home)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover kept: %v", err)
	}
}
