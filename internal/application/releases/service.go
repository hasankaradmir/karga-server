// Package releases implements the release lifecycle: install, upgrade, rollback, uninstall and reads.
package releases

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/runtimeops"

	"github.com/hasankaradmir/karga-sdk/packages"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// Errors callers can check.
var (
	ErrLocked      = errors.New("another operation is running on this release")
	ErrInterrupted = errors.New("a previous operation on this release was interrupted")
	ErrExists      = errors.New("release already exists")
)

// DefaultHistoryMax is how many revisions are kept per release unless a request says otherwise (D13).
const DefaultHistoryMax = 10

// historyMax is the number of revisions a request keeps (0: all).
func historyMax(n *int) int {
	if n == nil {
		return DefaultHistoryMax
	}
	return *n
}

// invalid marks an error as the caller's (a package that does not render, values that do not
// validate): the client sees it as api.ErrInvalid. The message is unchanged.
type invalid struct{ error }

func (e invalid) Unwrap() error      { return e.error }
func (invalid) Is(target error) bool { return target == api.ErrInvalid }

// Service runs release use cases. Runtime is opened lazily so read-only commands never need Docker.
type Service struct {
	// AllowedCaps are the Linux capabilities, besides resource.DefaultCaps, the host administrator
	// lets packages add (karga-server --allow-capabilities).
	AllowedCaps []string
	Store       ports.StateStore
	Runtime     func(context.Context) (ports.Runtime, error)
	Home        string
	// Redact registers secret values with the log redactor.
	Redact   func(string)
	Log      *slog.Logger
	Now      func() time.Time
	PID      int
	Hostname string

	// AfterChange, if set, is told the name of a release an install, upgrade or rollback changed.
	AfterChange func(name string)

	keyMu   sync.Mutex
	key     []byte // nil until loaded: a failed load is tried again next time
	repairs repairs
	// busy serializes the operations of a release inside this process: a user's command waits for
	// a reconciliation in progress instead of failing; a reconciliation skips a busy release.
	busy sync.Map // release -> chan struct{} (capacity 1: held while full)
}

// lock takes release rel for one operation, from reading its state to recording the outcome, so
// what an operation plans cannot go stale before it runs. It waits until rel is free or ctx ends;
// with try it does not wait. Call the returned function to let go.
func (s *Service) lock(ctx context.Context, rel string, try bool) (func(), error) {
	v, _ := s.busy.LoadOrStore(rel, make(chan struct{}, 1))
	ch := v.(chan struct{})
	unlock := func() { <-ch }
	select {
	case ch <- struct{}{}:
		return unlock, nil
	default:
	}
	if try {
		return nil, fmt.Errorf("%w: a command is working on %s", ErrLocked, rel)
	}
	select {
	case ch <- struct{}{}:
		return unlock, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the operation running on %s: %w", rel, ctx.Err())
	}
}

// New returns a Service with process defaults filled in.
func New(store ports.StateStore, rt func(context.Context) (ports.Runtime, error), home string, redact func(string), log *slog.Logger) *Service {
	host, _ := os.Hostname()
	if redact == nil {
		redact = func(string) {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{Store: store, Runtime: rt, Home: home, Redact: redact, Log: log, Now: time.Now, PID: os.Getpid(), Hostname: host}
}

// CommonOptions is shared with the CLI (karga-sdk/api).
type CommonOptions = api.CommonOptions

// Result is shared with the CLI (karga-sdk/api).
type Result = api.Result

// processAlive reports whether pid runs on this host (signal 0 only checks existence).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func describe(op release.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s of revision %d started %s by pid %d on %s", op.Type, op.Revision, op.StartedAt.Local().Format(time.RFC3339), op.PID, op.Hostname)
	for _, s := range op.Steps {
		fmt.Fprintf(&b, "\n  %s %s %s: %s", s.Action, s.Kind, s.Name, s.Status)
		if s.Error != "" {
			fmt.Fprintf(&b, " (%s)", s.Error)
		}
	}
	return b.String()
}

// begin records a running operation; the caller holds the release's lock. A live process holding the
// release always wins; a dead one is only overridden with force (D15).
func (s *Service) begin(ctx context.Context, rel, typ string, revision int, force bool) (release.Operation, error) {
	op := release.Operation{Release: rel, Type: typ, Revision: revision, Status: release.OperationRunning,
		PID: s.PID, Hostname: s.Hostname, Actor: actorOf(ctx), StartedAt: s.Now()}
	id, err := s.Store.BeginOperation(ctx, op, func(running release.Operation) error {
		// Under the release mutex no operation of this process runs: one with its PID is from an
		// earlier process that got the same PID (a container's PID 1).
		if running.Hostname == s.Hostname && running.PID != s.PID && processAlive(running.PID) {
			return fmt.Errorf("%w: %s", ErrLocked, describe(running))
		}
		if !force {
			return fmt.Errorf("%w: %s\nInspect with 'karga status %s'; rerun with --force to continue", ErrInterrupted, describe(running), rel)
		}
		s.Log.Warn("aborting interrupted operation", "release", rel, "operation", running.ID)
		return nil
	})
	op.ID = id
	if err == nil {
		s.Log.Info("operation", "operation_id", id, "release", rel, "type", typ, "revision", revision, "user", op.Actor)
	}
	return op, err
}

type actorKey struct{}

// WithActor tells the operations started with ctx who asked for them.
func WithActor(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, actorKey{}, user)
}

// ServerActor is the actor of what karga-server does on its own: repairs and resumed operations.
const ServerActor = "karga-server"

func actorOf(ctx context.Context) string {
	if u, _ := ctx.Value(actorKey{}).(string); u != "" {
		return u
	}
	return ServerActor
}

// finish records the end of an operation. It uses a context that survives cancellation (Ctrl-C) so
// the operation is not left running.
func (s *Service) finish(ctx context.Context, op release.Operation, err error) {
	ctx = context.WithoutCancel(ctx)
	op.Status = release.OperationSucceeded
	if err != nil {
		op.Status, op.Error = release.OperationFailed, err.Error()
	}
	if ferr := s.Store.FinishOperation(ctx, op); ferr != nil {
		s.Log.Error("record operation result", "release", op.Release, "error", ferr)
	}
}

// digestJSON is the keyed digest of v's JSON: values and manifests may hold secret material.
func digestJSON(v any) (string, []byte, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	return deployment.Digest(js), js, nil
}

// keyFile holds the key of the state database's digests of secret material (R7).
const keyFile = "state.key"

// loadKey reads $KARGA_HOME/state.key, creating it (32 random bytes, 0600) on first use, and makes
// it the digest key. Losing it only means secrets are re-applied once.
func (s *Service) loadKey() error {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.key == nil {
		path := filepath.Join(s.Home, keyFile)
		key, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			key = make([]byte, 32)
			if _, err = rand.Read(key); err == nil {
				err = writeNewFile(path, key)
				if errors.Is(err, os.ErrExist) { // created meanwhile
					key, err = os.ReadFile(path)
				}
			}
		}
		if err == nil && len(key) < 16 {
			err = fmt.Errorf("%s is too short; remove it to create a new key", path)
		}
		if err != nil {
			return fmt.Errorf("state key: %w", err)
		}
		s.key = key
	}
	// shortcut: the digest key is process-wide; services of different homes in one process must
	// not plan concurrently (karga-server runs one). Pass the key down if that changes.
	deployment.SetDigestKey(s.key)
	return nil
}

// writeNewFile creates path with data atomically: the data is written to a temporary file that is
// then linked into place, so a concurrent reader never sees it half written. It fails with
// os.ErrExist when path exists.
func writeNewFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// On disk before it is linked: after a power loss the file is whole or absent, never empty.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the entries of dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }() // read-only
	return d.Sync()
}

// encodeManifest stores resources as a JSON array with secret values replaced by digests.
func encodeManifest(rs []resource.Resource) (json.RawMessage, string, error) {
	redacted := make([]resource.Resource, len(rs))
	for i, r := range rs {
		redacted[i] = deployment.Redacted(r)
	}
	d, js, err := digestJSON(redacted)
	return js, d, err
}

// decodeManifest parses a stored manifest. Secret values are still digests; see restoreSecrets.
func decodeManifest(js json.RawMessage) ([]resource.Resource, error) {
	var docs []json.RawMessage
	if err := json.Unmarshal(js, &docs); err != nil {
		return nil, err
	}
	rs := make([]resource.Resource, len(docs))
	for i, d := range docs {
		r, err := resource.Decode(d) // JSON is YAML
		if err != nil {
			return nil, err
		}
		rs[i] = r
	}
	return rs, nil
}

// restoreSecrets fills secret values from the files materialized for that revision.
func (s *Service) restoreSecrets(rel string, revision int, rs []resource.Resource) error {
	dir := deployment.RevisionDir(s.Home, rel, revision)
	for i, r := range rs {
		sec, ok := r.Spec.(*resource.SecretSpec)
		if !ok {
			continue
		}
		values := map[string]string{}
		for k := range sec.Values {
			b, err := os.ReadFile(filepath.Join(dir, "secrets", r.Metadata.Name, k))
			if err != nil {
				return fmt.Errorf("revision %d: secret %q is no longer available (%v); it may have been pruned", revision, r.Metadata.Name, err)
			}
			values[k] = string(b)
		}
		rs[i].Spec = &resource.SecretSpec{Values: values}
	}
	return nil
}

// deployInput is a desired revision, from install, upgrade or rollback.
type deployInput struct {
	CommonOptions
	Name          string
	Type          string // install, upgrade, rollback
	Description   string
	Package       packages.Metadata
	PackageDigest string
	Overrides     map[string]any
	Values        map[string]any
	Resources     []resource.Resource
	Revision      int
	// Strategy overrides the workloads' updateStrategy ("" = use the spec).
	Strategy string
	// SecretPaths are writeOnly values: stored as <redacted>, never in plain text.
	SecretPaths [][]string
	// Existing is the current release row, nil for a new release.
	Existing *release.Release
	// Notes is the rendered NOTES.txt (writeOnly values redacted).
	Notes string
	// Progress, if set, receives what the deployment is doing as it happens.
	Progress func(api.Event)
	// resumed deploys a revision that is already recorded, for an operation a stopped karga-server left.
	resumed bool
}

func (s *Service) nextRevision(ctx context.Context, name string) (int, error) {
	revs, err := s.Store.ListRevisions(ctx, name)
	if err != nil {
		return 0, err
	}
	if len(revs) == 0 {
		return 1, nil
	}
	return revs[len(revs)-1].Number + 1, nil
}

var inProgress = []release.Status{release.StatusPending, release.StatusDeploying, release.StatusUpgrading, release.StatusUninstalling}

// deploy plans the desired revision against the applied state and executes it.
// runtime connects to the container runtime and tells the deployment package whether it can run
// karga's proxies there.
func (s *Service) runtime(ctx context.Context) (ports.Runtime, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	info, err := rt.Info(ctx)
	if err != nil {
		return nil, err
	}
	runtimeops.UseProxies(info)
	return rt, nil
}

func (s *Service) deploy(ctx context.Context, in deployInput) (*Result, error) {
	if err := s.loadKey(); err != nil {
		return nil, err
	}
	applied, err := s.Store.ListResources(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	// A dry run plans without a runtime if there is none (as if proxies were unavailable).
	rt, rtErr := s.runtime(ctx)
	if rtErr != nil && !in.DryRun {
		return nil, rtErr
	}
	recorded := applied
	var anon deployment.Anonymous
	if rtErr == nil {
		if applied, err = existing(ctx, rt, in.Name, applied); err != nil {
			return nil, err
		}
		if anon, err = anonymousVolumes(ctx, rt, in.Name, in.Resources); err != nil {
			return nil, err
		}
	}
	plan, err := deployment.Diff(in.Name, in.Resources, applied, in.Strategy, anon)
	if err != nil {
		return nil, err
	}
	res := &Result{Release: in.Name, Revision: in.Revision, Plan: plan.Wire(), DryRun: in.DryRun, Notes: in.Notes}
	if in.Existing != nil {
		res.From = in.Existing.CurrentRevision
	}
	if in.DryRun {
		res.Warnings = anonymousWarnings(anon)
		if gone := plan.DeletedVolumes(); len(gone) > 0 && !in.DeleteVolumes {
			res.Warnings = append(res.Warnings, fmt.Sprintf("this %s deletes volume %s and its data; it needs --delete-volumes", in.Type, strings.Join(gone, ", ")))
		}
		return res, nil
	}
	if gone := plan.DeletedVolumes(); len(gone) > 0 && !in.DeleteVolumes {
		return nil, fmt.Errorf("this %s deletes volume %s and its data (retention: delete); nothing was changed. Rerun with --delete-volumes to go ahead",
			in.Type, strings.Join(gone, ", "))
	}

	opType := in.Type
	if in.resumed {
		opType = ResumeOperation
	}
	op, err := s.begin(ctx, in.Name, opType, in.Revision, in.Force)
	if err != nil {
		return nil, err
	}
	if plan, err = s.reconcile(ctx, rt, in, recorded, plan); err == nil {
		res.Plan = plan.Wire()
		err = s.run(ctx, rt, &op, in, plan)
	}
	s.finish(ctx, op, err)
	if err != nil {
		res.Status = string(release.StatusFailed)
		hint := fmt.Sprintf("revision %d failed", in.Revision)
		var execErr *deployment.ExecError
		switch {
		case errors.As(err, &execErr) && !execErr.Disrupted && res.From > 0:
			res.Status = string(in.Existing.Status)
			hint += fmt.Sprintf("; nothing that was running changed and revision %d is still serving", res.From)
		case res.From > 0:
			hint += fmt.Sprintf("; revision %d stays current. Inspect with 'karga status %s' (and 'docker logs'), then fix and upgrade or run 'karga rollback %s'", res.From, in.Name, in.Name)
		case s.discard(context.WithoutCancel(ctx), in.Name):
			hint += "; nothing was installed. Fix the problem and run 'karga install' again"
		default:
			hint += fmt.Sprintf("; fix the problem and run 'karga install' again, or remove the failed release with 'karga uninstall %s'", in.Name)
		}
		return res, fmt.Errorf("%w\n%s", err, hint)
	}
	res.Status = string(release.StatusDeployed)
	s.repairs.reset(in.Name)
	if s.AfterChange != nil {
		s.AfterChange(in.Name)
	}
	if anon, err := anonymousVolumes(ctx, rt, in.Name, in.Resources); err == nil {
		res.Warnings = anonymousWarnings(anon)
	}
	if perr := s.prune(context.WithoutCancel(ctx), in.Name, historyMax(in.HistoryMax)); perr != nil {
		s.Log.Warn("prune history", "release", in.Name, "error", perr)
	}
	return res, nil
}

// reconcile repairs leftovers of an interrupted replace and, if anything changed, plans again.
// discard removes a release that never had a deployed revision and owns nothing in the runtime (an
// install that failed before creating anything), so it does not linger as a failed release.
func (s *Service) discard(ctx context.Context, name string) bool {
	applied, err := s.Store.ListResources(ctx, name)
	if err != nil || len(applied) > 0 {
		return false
	}
	if err := s.Store.DeleteRelease(ctx, name); err != nil {
		s.Log.Warn("remove failed release", "release", name, "error", err)
		return false
	}
	return true
}

// anonymousVolumes records on each workload the image VOLUME paths its container keeps in anonymous
// volumes. Such data moves to the new container on a recreate but cannot be shared by two, so these
// workloads are never replaced.
// anonymousVolumes finds the image VOLUME paths the release's current workload containers keep in
// anonymous volumes.
func anonymousVolumes(ctx context.Context, rt ports.Runtime, name string, rs []resource.Resource) (deployment.Anonymous, error) {
	anon := deployment.Anonymous{}
	for _, r := range rs {
		if r.Kind != resource.KindWorkload {
			continue
		}
		st, err := rt.GetWorkload(ctx, runtimeops.RuntimeName(name, r.Metadata.Name))
		if errors.Is(err, ports.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, m := range st.AnonymousVolumes {
			anon[r.Metadata.Name] = append(anon[r.Metadata.Name], m.Target)
		}
	}
	return anon, nil
}

func anonymousWarnings(anon deployment.Anonymous) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(anon)) {
		out = append(out, fmt.Sprintf("workload %s keeps image VOLUME %s in an anonymous volume: upgrades keep it, but uninstall deletes it and it is not in volume backups. Mount a Volume there.",
			name, strings.Join(anon[name], ", ")))
	}
	return out
}

// existing drops the recorded workloads, proxies, networks and volumes that no longer exist in the
// runtime (removed outside karga, or a state restored on another host), and the workloads whose
// container was created from another spec (changed outside karga), so the plan creates them again.
func existing(ctx context.Context, rt ports.Runtime, name string, applied []release.AppliedResource) ([]release.AppliedResource, error) {
	objs, err := rt.ListManaged(ctx, name)
	if err != nil {
		return nil, err
	}
	have := map[string]map[string]string{}
	for _, o := range objs {
		have[o.Kind+"/"+o.Name] = o.Labels
	}
	kinds := map[string]string{resource.KindWorkload: ports.ObjectWorkload, deployment.KindProxy: ports.ObjectWorkload,
		resource.KindNetwork: ports.ObjectNetwork, resource.KindVolume: ports.ObjectVolume}
	var out []release.AppliedResource
	for _, a := range applied {
		k, ok := kinds[a.Kind]
		lbl, present := have[k+"/"+a.RuntimeName]
		if ok && !present {
			continue
		}
		// Containers of an earlier karga have no digest label: they are not drift.
		if d := lbl[ports.LabelDigest]; a.Kind == resource.KindWorkload && d != "" && a.Digest != "" && d != a.Digest {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) reconcile(ctx context.Context, rt ports.Runtime, in deployInput, applied []release.AppliedResource, plan deployment.Plan) (deployment.Plan, error) {
	repaired, err := deployment.Reconcile(ctx, rt, s.Home, in.Name, applied)
	if err != nil || len(repaired) == 0 {
		return plan, err
	}
	for _, a := range repaired {
		s.Log.Warn("repaired interrupted replace", "release", in.Name, "resource", a.Kind+"/"+a.Name)
		if err := s.Store.SaveResource(ctx, in.Name, a); err != nil {
			return plan, err
		}
	}
	if applied, err = s.Store.ListResources(ctx, in.Name); err != nil {
		return plan, err
	}
	if applied, err = existing(ctx, rt, in.Name, applied); err != nil {
		return plan, err
	}
	anon, err := anonymousVolumes(ctx, rt, in.Name, in.Resources)
	if err != nil {
		return plan, err
	}
	return deployment.Diff(in.Name, in.Resources, applied, in.Strategy, anon)
}

func (s *Service) run(ctx context.Context, rt ports.Runtime, op *release.Operation, in deployInput, plan deployment.Plan) error {
	now := s.Now()
	rel := release.Release{Name: in.Name, PackageName: in.Package.Name, PackageVersion: in.Package.Version,
		Status: release.StatusPending, CreatedAt: now, UpdatedAt: now}
	target := release.StatusDeploying
	if in.Existing != nil {
		rel = *in.Existing
		if slices.Contains(inProgress, rel.Status) {
			// Left behind by an operation that begin just aborted.
			rel.Status = release.StatusFailed
		}
		if in.Type != "install" {
			target = release.StatusUpgrading
		}
	}
	if err := release.Transition(rel.Status, target); err != nil {
		return err
	}
	previous := rel.Status
	rel.Status, rel.UpdatedAt = target, now
	manifest, manifestDigest, err := encodeManifest(in.Resources)
	if err != nil {
		return err
	}
	valuesDigest, _, err := digestJSON(in.Values)
	if err != nil {
		return err
	}
	// The release's new status and its new revision are recorded together: a release is never left
	// deploying a revision that does not exist.
	err = s.Store.InTx(ctx, func(st ports.StateStore) error {
		if err := st.SaveRelease(ctx, rel); err != nil {
			return err
		}
		if in.resumed { // a resumed operation's revision is already recorded
			return nil
		}
		return st.CreateRevision(ctx, release.Revision{
			Release: in.Name, Number: in.Revision, PackageName: in.Package.Name, PackageVersion: in.Package.Version,
			RuntimeSupported: in.Package.Runtime.Supported, PackageDigest: in.PackageDigest, ValuesDigest: valuesDigest, ManifestDigest: manifestDigest,
			Values: maskValues(in.Overrides, in.SecretPaths), ValuesAll: maskValues(in.Values, in.SecretPaths), Manifest: manifest,
			Status: release.RevisionPending, Description: in.Description, CreatedAt: now, Notes: in.Notes,
		})
	})
	if err != nil {
		return err
	}

	execErr := deployment.Execute(ctx, rt, deployment.ExecRequest{
		Home: s.Home, Release: in.Name, Revision: in.Revision, Supported: in.Package.Runtime.Supported,
		Resources: in.Resources, Plan: plan, Redact: s.Redact, Timeout: time.Duration(in.Timeout), AllowedCaps: s.AllowedCaps,
		OnStep:     func(c deployment.Change, st release.Step) error { return s.record(ctx, op, in, c, st) },
		OnProgress: in.Progress,
	})

	bg := context.WithoutCancel(ctx)
	rel.UpdatedAt = s.Now()
	if execErr != nil {
		rel.Status = release.StatusFailed
		// A failed replace that touched nothing leaves the release as it was (D18).
		var ee *deployment.ExecError
		if errors.As(execErr, &ee) && !ee.Disrupted && in.Existing != nil {
			rel.Status = previous
		}
		desc := fmt.Sprintf("%s failed: %v", titled(in.Type), execErr)
		return errors.Join(execErr, s.settle(bg, rel, in.Revision, release.RevisionFailed, desc))
	}
	rel.Status, rel.CurrentRevision = release.StatusDeployed, in.Revision
	rel.PackageName, rel.PackageVersion = in.Package.Name, in.Package.Version
	return s.settle(bg, rel, in.Revision, release.RevisionDeployed, in.Description)
}

// settle records the outcome of a revision and the release it leaves, together.
func (s *Service) settle(ctx context.Context, rel release.Release, revision int, status release.RevisionStatus, desc string) error {
	return s.Store.InTx(ctx, func(st ports.StateStore) error {
		if err := st.UpdateRevisionStatus(ctx, rel.Name, revision, status, desc); err != nil {
			return err
		}
		return st.SaveRelease(ctx, rel)
	})
}

func titled(t string) string { return strings.ToUpper(t[:1]) + t[1:] }

// record persists one executor step: progress on the operation and the applied state of the resource.
// A failed workload is stored with an empty digest so the next plan recreates it; a kept-previous
// step (failed replace) leaves the applied state alone.
func (s *Service) record(ctx context.Context, op *release.Operation, in deployInput, c deployment.Change, st release.Step) error {
	ctx = context.WithoutCancel(ctx)
	op.Steps = append(op.Steps, st)
	s.Log.Debug("step", "operation_id", op.ID, "release", in.Name, "resource", c.Kind+"/"+c.Name, "action", st.Action, "status", st.Status)
	if err := s.Store.SaveOperationSteps(ctx, op.ID, op.Steps); err != nil {
		return err
	}
	runtimeName := c.RuntimeName
	if runtimeName == "" {
		runtimeName = runtimeops.RuntimeName(in.Name, c.Name)
	}
	applied := release.AppliedResource{Kind: c.Kind, Name: c.Name, Revision: in.Revision,
		RuntimeName: runtimeName, Spec: c.Spec, Digest: c.Digest}
	switch {
	case st.Status == deployment.StepKept:
		// The change failed before touching anything; the previous state is still what runs.
	case st.Status == deployment.StepDone && c.Action == deployment.ActionDelete:
		return s.Store.DeleteResource(ctx, in.Name, c.Kind, c.Name)
	case st.Status == deployment.StepDone:
		return s.Store.SaveResource(ctx, in.Name, applied)
	case (c.Kind == resource.KindWorkload || c.Kind == deployment.KindProxy) && c.Spec != nil:
		applied.Digest = ""
		return s.Store.SaveResource(ctx, in.Name, applied)
	}
	return nil
}

// prune keeps the newest historyMax revisions. The current revision and any revision whose files are still
// mounted by an applied resource are never removed (D13).
func (s *Service) prune(ctx context.Context, name string, historyMax int) error {
	if historyMax <= 0 {
		return nil
	}
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return err
	}
	revs, err := s.Store.ListRevisions(ctx, name)
	if err != nil {
		return err
	}
	applied, err := s.Store.ListResources(ctx, name)
	if err != nil {
		return err
	}
	keep := map[int]bool{rel.CurrentRevision: true}
	for _, a := range applied {
		keep[a.Revision] = true
	}
	var errs []error
	// revs is ascending: everything before the newest max is a candidate.
	for _, r := range revs[:max(0, len(revs)-historyMax)] {
		if keep[r.Number] {
			continue
		}
		errs = append(errs, s.Store.DeleteRevision(ctx, name, r.Number),
			os.RemoveAll(deployment.RevisionDir(s.Home, name, r.Number)))
	}
	return errors.Join(errs...)
}
