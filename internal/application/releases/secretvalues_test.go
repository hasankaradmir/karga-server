package releases

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-sdk/packages"
)

func TestMaskDropHasValues(t *testing.T) {
	m := map[string]any{"database": map[string]any{"password": "hunter2", "user": "app"}, "token": nil}
	paths := [][]string{{"database", "password"}, {"token"}, {"missing", "x"}}
	masked := packages.MaskValues(m, paths)
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

// writeOnly values under maps and lists (paths with *) are stored masked, and --reuse-values asks
// for them again.
func TestWriteOnlyUnderMapsAndListsNotStored(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := scratchPkg(t)
	schema := `{"type":"object","properties":{
	  "users":{"type":"object","additionalProperties":{"type":"object","properties":{"password":{"type":"string","writeOnly":true}}}},
	  "keys":{"type":"array","items":{"type":"object","properties":{"token":{"type":"string","writeOnly":true}}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "values.schema.json"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	install := func(reuse bool, sets ...string) error {
		var err error
		if reuse {
			_, err = e.svc.Lifecycle.Upgrade(ctx, UpgradeRequest{CommonOptions: common(), Name: "w", Package: dir, ReuseValues: true, Values: Values{Sets: packages.Sets(sets...)}}, nil)
		} else {
			_, err = e.svc.Lifecycle.Install(ctx, InstallRequest{CommonOptions: common(), Name: "w", Package: dir, Values: Values{Sets: packages.Sets(sets...)}}, nil)
		}
		return err
	}
	if err := install(false, "users.alice.password=alice-secret-1", "keys[0].token=token-secret-1"); err != nil {
		t.Fatal(err)
	}
	rev, err := e.svc.Store.GetRevision(ctx, "w", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, vals := range []map[string]any{rev.Values, rev.ValuesAll} {
		b, _ := json.Marshal(vals)
		if strings.Contains(string(b), "secret-1") {
			t.Fatalf("a writeOnly value was stored: %s", b)
		}
	}
	if err := install(true); err == nil || !strings.Contains(err.Error(), "writeOnly") {
		t.Fatalf("--reuse-values without the writeOnly values: %v", err)
	}
	if err := install(true, "users.alice.password=alice-secret-2", "keys[0].token=token-secret-2"); err != nil {
		t.Fatalf("--reuse-values with them again: %v", err)
	}
}
