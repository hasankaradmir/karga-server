package releases

import (
	"reflect"
	"strings"
	"testing"
)

func TestMaskDropHasValues(t *testing.T) {
	m := map[string]any{"database": map[string]any{"password": "hunter2", "user": "app"}, "token": nil}
	paths := [][]string{{"database", "password"}, {"token"}, {"missing", "x"}}
	masked := maskValues(m, paths)
	want := map[string]any{"database": map[string]any{"password": "<redacted>", "user": "app"}, "token": nil}
	if !reflect.DeepEqual(masked, want) {
		t.Fatalf("mask: %#v", masked)
	}
	if m["database"].(map[string]any)["password"] != "hunter2" {
		t.Fatal("Mask modified its input")
	}
	if rp := redactedPaths(masked); len(rp) != 1 || strings.Join(rp[0], ".") != "database.password" {
		t.Fatalf("redacted paths: %v", rp)
	}
	dropped := dropValues(masked, redactedPaths(masked))
	if hasValue(dropped, []string{"database", "password"}) || !hasValue(dropped, []string{"database", "user"}) || hasValue(m, []string{"token"}) {
		t.Fatalf("drop/has: %#v", dropped)
	}
}
