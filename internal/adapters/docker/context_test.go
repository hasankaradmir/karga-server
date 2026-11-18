package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeContext(t *testing.T, dir, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestContextHost(t *testing.T) {
	dir := t.TempDir()
	writeContext(t, dir, "orbstack", "unix:///orb.sock")
	writeContext(t, dir, "colima", "unix:///colima.sock")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"orbstack"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"DOCKER_HOST wins", map[string]string{"DOCKER_HOST": "tcp://x:2375", "DOCKER_CONFIG": dir, "DOCKER_CONTEXT": "colima"}, ""},
		{"DOCKER_CONTEXT", map[string]string{"DOCKER_CONFIG": dir, "DOCKER_CONTEXT": "colima"}, "unix:///colima.sock"},
		{"currentContext", map[string]string{"DOCKER_CONFIG": dir}, "unix:///orb.sock"},
		{"default context", map[string]string{"DOCKER_CONFIG": dir, "DOCKER_CONTEXT": "default"}, ""},
		{"no config", map[string]string{"DOCKER_CONFIG": t.TempDir()}, ""},
		{"HOME fallback", map[string]string{"HOME": t.TempDir()}, ""},
	}
	for _, tt := range tests {
		got, _, err := contextHost(func(k string) string { return tt.env[k] })
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	if _, _, err := contextHost(func(k string) string {
		return map[string]string{"DOCKER_CONFIG": dir, "DOCKER_CONTEXT": "missing"}[k]
	}); err == nil {
		t.Error("unknown context: expected error")
	}
}

func TestRootless(t *testing.T) {
	if !rootless([]string{"name=seccomp,profile=builtin", "name=rootless", "name=cgroupns"}) || rootless([]string{"name=seccomp,profile=builtin"}) {
		t.Fatal("rootless detection")
	}
}
