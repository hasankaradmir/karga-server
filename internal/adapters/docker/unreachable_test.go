package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

func TestNewUnreachable(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent.sock")
	_, err := New(context.Background())
	if !errors.Is(err, ports.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"cannot reach Docker at unix:///nonexistent.sock", "no such file or directory", "is the daemon running?"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if strings.Count(msg, "nonexistent.sock") > 1 {
		t.Errorf("host repeated: %q", msg)
	}
}

func TestNewUnreachableNamesContext(t *testing.T) {
	dir := t.TempDir()
	writeContext(t, dir, "colima", "unix:///nonexistent-karga.sock")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", dir)
	_, err := New(context.Background())
	if err == nil || !strings.Contains(err.Error(), `(docker context "colima")`) {
		t.Fatalf("context not named: %v", err)
	}
}
