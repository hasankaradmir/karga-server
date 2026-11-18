package releases

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/domain/release"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// List returns every release.
func (s *Reads) List(ctx context.Context) ([]release.Release, error) {
	return s.c.store.ListReleases(ctx)
}

// History returns a release's revisions, oldest first.
func (s *Reads) History(ctx context.Context, name string) ([]release.Revision, error) {
	if _, err := s.c.store.GetRelease(ctx, name); err != nil {
		return nil, err
	}
	revs, err := s.c.store.ListRevisions(ctx, name)
	if err != nil {
		return nil, err
	}
	// Only the newest deployed revision is what runs (a failed one after it changed nothing that
	// stayed); earlier deployed ones are shown as superseded. The state keeps them deployed:
	// rollback looks for them.
	newest := true
	for i := len(revs) - 1; i >= 0; i-- {
		if revs[i].Status == release.RevisionDeployed {
			if !newest {
				revs[i].Status = release.RevisionSuperseded
			}
			newest = false
		}
	}
	return revs, nil
}

// revisionOrCurrent resolves revision 0 to the current revision.
func (s *core) revisionOrCurrent(ctx context.Context, name string, number int) (release.Revision, error) {
	if number == 0 {
		rel, err := s.store.GetRelease(ctx, name)
		if err != nil {
			return release.Revision{}, err
		}
		if number = rel.CurrentRevision; number == 0 {
			return release.Revision{}, fmt.Errorf("release %s has no deployed revision", name)
		}
	}
	return s.store.GetRevision(ctx, name, number)
}

// Values returns the user overrides of a revision (0 = current), or with all the merged values.
func (s *Reads) Values(ctx context.Context, name string, revision int, all bool) (map[string]any, error) {
	rev, err := s.c.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return nil, err
	}
	if all {
		return rev.ValuesAll, nil
	}
	return rev.Values, nil
}

// Manifest returns a revision's resources with secret values shown as <redacted>.
// Notes returns the rendered NOTES.txt of a revision (0: the current one), "" if the package has none.
func (s *Reads) Notes(ctx context.Context, name string, revision int) (string, error) {
	rev, err := s.c.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return "", err
	}
	return rev.Notes, nil
}

func (s *Reads) Manifest(ctx context.Context, name string, revision int) ([]resource.Resource, error) {
	rev, err := s.c.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return nil, err
	}
	rs, err := decodeManifest(rev.Manifest)
	if err != nil {
		return nil, err
	}
	for i, r := range rs {
		if sec, ok := r.Spec.(*resource.SecretSpec); ok {
			masked := &resource.SecretSpec{Values: map[string]string{}}
			for k := range sec.Values {
				masked.Values[k] = "<redacted>"
			}
			rs[i].Spec = masked
		}
	}
	return rs, nil
}

// Status reports the release and the live state of each of its resources.
func (s *Reads) Status(ctx context.Context, name string) (*StatusReport, error) {
	rel, err := s.c.store.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{Release: rel, Resources: []ResourceStatus{}}
	if op, err := s.c.store.RunningOperation(ctx, name); err == nil {
		rep.Interrupted = describe(op)
		if op.Hostname == s.c.hostname && op.PID == s.c.pid { // this server runs it now
			rep.Interrupted = "in progress: " + rep.Interrupted
		}
	} else if !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	rep.Reconcile = &ReconcileStatus{GaveUp: s.c.repairs.gaveUp(name, s.c.now())}
	if ops, err := s.c.store.ListOperations(ctx, name, 0, 50); err == nil {
		for _, op := range ops {
			if op.Type == ReconcileOperation && op.Status != release.OperationRunning {
				rep.Reconcile.Last, rep.Reconcile.Status, rep.Reconcile.Error = op.FinishedAt, string(op.Status), op.Error
				break
			}
		}
	}
	applied, err := s.c.store.ListResources(ctx, name)
	if err != nil {
		return nil, err
	}
	objs, err := s.c.rt.ListManaged(ctx, name)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, o := range objs {
		present[o.Kind+"/"+o.Name] = true
	}
	tracked := map[string]bool{}
	for _, a := range applied {
		st, err := s.resourceStatus(ctx, name, a, present, tracked)
		if err != nil {
			return nil, err
		}
		rep.Resources = append(rep.Resources, st)
	}
	for _, o := range objs {
		// Volumes outlive uninstalls on purpose; only report unexpected workloads and networks.
		if !tracked[o.Kind+"/"+o.Name] && o.Kind != ports.ObjectVolume {
			rep.Orphans = append(rep.Orphans, o.Kind+"/"+o.Name)
		}
	}
	return rep, nil
}

// resourceStatus compares applied resource a of release name with the runtime. present holds the
// release's runtime objects ("kind/name"); the ones a accounts for are added to tracked.
func (s *Reads) resourceStatus(ctx context.Context, name string, a release.AppliedResource, present, tracked map[string]bool) (ResourceStatus, error) {
	st := ResourceStatus{Kind: a.Kind, Name: a.Name, RuntimeName: a.RuntimeName, Revision: a.Revision, Desired: "applied"}
	if a.Digest == "" {
		st.Desired = "failed"
	}
	switch a.Kind {
	case resource.KindWorkload, deployment.KindProxy:
		tracked[ports.ObjectWorkload+"/"+a.RuntimeName] = true
		ws, err := s.c.rt.GetWorkload(ctx, a.RuntimeName)
		switch {
		case errors.Is(err, ports.ErrNotFound):
			st.Actual = "missing"
		case err != nil:
			return st, err
		default:
			st.Actual, st.Health, st.Restarts, st.OOMKilled = ws.State, ws.Health, ws.RestartCount, ws.OOMKilled
			if r, derr := resource.DecodeStored(a.Spec); a.Kind == resource.KindWorkload && derr == nil {
				if w, ok := r.Spec.(*resource.WorkloadSpec); ok {
					st.Health = s.c.env.ProbeHealth(ctx, ws, w) // tcp/http checks are karga's
				}
			}
		}
		if a.Kind == deployment.KindProxy {
			st.Backend = s.backends(deployment.ProxyDir(s.c.home, name), "")
		}
	case resource.KindService:
		// A Service has no container: it is routed when the release proxy sends its ports somewhere.
		st.Actual = "not routed"
		if b := s.backends(deployment.ProxyDir(s.c.home, name), a.Name+"-p"); b != "" {
			st.Actual, st.Backend = "routed", b
		}
	case resource.KindNetwork, resource.KindVolume:
		kind := ports.ObjectNetwork
		if a.Kind == resource.KindVolume {
			kind = ports.ObjectVolume
		}
		tracked[kind+"/"+a.RuntimeName] = true
		st.Actual = "missing"
		if present[kind+"/"+a.RuntimeName] {
			st.Actual = "present"
		}
	default:
		st.Actual = "file" // Config and Secret live under the revision directory
	}
	return st, nil
}

// backends lists the aliases a release proxy routes to, e.g. "karga-api-r2, karga-web-r3", for the
// clusters whose name starts with prefix.
func (s *Reads) backends(dir, prefix string) string {
	eps, _ := s.c.env.Host().Proxies.Endpoints(dir)
	var out []string
	for cluster, ep := range eps {
		if !strings.HasPrefix(cluster, prefix) {
			continue
		}
		host, _, _ := net.SplitHostPort(ep)
		out = append(out, host)
	}
	slices.Sort(out)
	return strings.Join(slices.Compact(out), ", ")
}

// Diff compares revision from with revision to of a release (to 0: the current one).
func (s *Reads) Diff(ctx context.Context, name string, from, to int) (*RevisionDiff, error) {
	a, err := s.c.store.GetRevision(ctx, name, from)
	if err != nil {
		return nil, err
	}
	b, err := s.c.revisionOrCurrent(ctx, name, to)
	if err != nil {
		return nil, err
	}
	res, err := deployment.CompareManifests(a.Manifest, b.Manifest)
	if err != nil {
		return nil, err
	}
	d := &RevisionDiff{Release: name, From: a.Number, To: b.Number, Resources: res,
		Values: deployment.CompareValues(a.ValuesAll, b.ValuesAll)}
	if d.Resources == nil {
		d.Resources = []deployment.ResourceDiff{}
	}
	if a.PackageName != b.PackageName || a.PackageVersion != b.PackageVersion {
		d.Package = []deployment.FieldChange{{Path: "package", Old: a.PackageName + " " + a.PackageVersion, New: b.PackageName + " " + b.PackageVersion}}
	}
	return d, nil
}

// Resources lists what is applied for a release (for completion and tools; no runtime is read).
func (s *Reads) Resources(ctx context.Context, name string) ([]release.AppliedResource, error) {
	return s.c.store.ListResources(ctx, name)
}

// Operations lists the operations of a release (all for ""), newest first.
func (s *Reads) Operations(ctx context.Context, name string, afterID int64, limit int) ([]release.Operation, error) {
	return s.c.store.ListOperations(ctx, name, afterID, limit)
}
