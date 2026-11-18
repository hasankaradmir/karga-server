package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.yaml")
	for _, content := range []string{"one", "two"} {
		if err := WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != content {
			t.Fatalf("content %q %v", got, err)
		}
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if err := WriteFile(filepath.Join(dir, "missing", "b"), nil, 0o600); err == nil {
		t.Fatal("a missing directory was not reported")
	}
}
