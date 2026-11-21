package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hasankaradmir/karga-server/internal/adapters/fakeruntime"
)

// slowPull pulls for 80ms, reporting 12MB of 31MB downloaded after 10ms.
type slowPull struct{ *fakeruntime.Runtime }

func (r slowPull) EnsureImage(ctx context.Context, ref, policy string, progress func(current, total int64)) error {
	time.Sleep(10 * time.Millisecond)
	if progress != nil {
		progress(12e6, 31e6)
	}
	time.Sleep(70 * time.Millisecond)
	return r.Runtime.EnsureImage(ctx, ref, policy, progress)
}

func TestPullReportsProgress(t *testing.T) {
	var events []Event
	ctx := withProgress(context.Background(), func(e Event) { events = append(events, e) })
	env := NewEnv(testHost, EnvConfig{WaitEvery: 25 * time.Millisecond})
	r := &run{e: env, rt: slowPull{fakeruntime.New()}}
	if err := r.pull(ctx, "Workload", "web", "gitea/gitea:1.27.2", "if-not-present"); err != nil {
		t.Fatal(err)
	}
	n := len(events)
	if n < 2 {
		t.Fatalf("want a line every 25ms of an 80ms pull, got %v", events)
	}
	for _, e := range events {
		if e.Kind != "Workload" || e.Name != "web" || e.Phase != PhasePull || !strings.HasPrefix(e.Detail, "pulling gitea/gitea:1.27.2: 12MB/31MB (") {
			t.Fatalf("event %+v", e)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if len(events) != n {
		t.Fatalf("progress after the pull returned: %v", events[n:])
	}
	if got := []string{bytesOf(999), bytesOf(12_400_000), bytesOf(2_340_000_000)}; strings.Join(got, " ") != "1kB 12MB 2.3GB" {
		t.Fatalf("bytesOf: %v", got)
	}
}
