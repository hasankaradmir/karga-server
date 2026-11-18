package releases

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/adapters/filestore"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

func TestOperationsRecordNoSecret(t *testing.T) {
	inner, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := maskingStore{inner, func(v string) string { return strings.ReplaceAll(v, "hunter2", "<redacted>") }}
	ctx := context.Background()
	op := release.Operation{Release: "db", Type: release.OpInstall, Revision: 1, Status: release.OperationRunning, StartedAt: time.Now()}
	id, err := s.BeginOperation(ctx, op, nil)
	if err != nil {
		t.Fatal(err)
	}
	op.ID = id
	if err := s.SaveOperationSteps(ctx, id, []release.Step{{Kind: "Workload", Name: "db", Error: "start: password hunter2 refused"}}); err != nil {
		t.Fatal(err)
	}
	op.Status, op.Error = release.OperationFailed, "connect with hunter2: refused"
	op.Steps = []release.Step{{Kind: "Workload", Name: "db", Error: "hunter2"}}
	if err := s.FinishOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	ops, err := inner.ListOperations(ctx, "db", 0, 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("operations: %v %v", ops, err)
	}
	if got := ops[0].Error + ops[0].Steps[0].Error; strings.Contains(got, "hunter2") {
		t.Fatalf("a secret was recorded: %q", got)
	}
}
