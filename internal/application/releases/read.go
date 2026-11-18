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
	return s.Store.ListReleases(ctx)
}

// History returns a release's revisions, oldest first.
func (s *Reads) History(ctx context.Context, name string) ([]release.Revision, error) {
	if _, err := s.Store.GetRelease(ctx, name); err != nil {
		return nil, err
	}
	return s.Store.ListRevisions(ctx, name)
}

// revisionOrCurrent resolves revision 0 to the current revision.
func (s *Core) revisionOrCurrent(ctx context.Context, name string, number int) (release.Revision, error) {
	if number == 0 {
		rel, err := s.Store.GetRelease(ctx, name)
		if err != nil {
			return release.Revision{}, err
		}
		if number = rel.CurrentRevision; number == 0 {
			return release.Revision{}, fmt.Errorf("release %s has no deployed revision", name)
		}
	}
	return s.Store.GetRevision(ctx, name, number)
}

// GetValues returns the user overrides of a revision (0 = current), or with all the merged values.
func (s *Reads) GetValues(ctx context.Context, name string, revision int, all bool) (map[string]any, error) {
	rev, err := s.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return nil, err
	}
	if all {
		return rev.ValuesAll, nil
	}
	return rev.Values, nil
}

// GetManifest returns a revision's resources with secret values shown as <redacted>.
// GetNotes returns the rendered NOTES.txt of a revision (0: the current one), "" if the package has none.
func (s *Reads) GetNotes(ctx context.Context, name string, revision int) (string, error) {
	rev, err := s.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return "", err
	}
	return rev.Notes, nil
}

func (s *Reads) GetManifest(ctx context.Context, name string, revision int) ([]resource.Resource, error) {
	rev, err := s.revisionOrCurrent(ctx, name, revision)
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
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{Release: rel, Resources: []ResourceStatus{}}
	if op, err := s.Store.RunningOperation(ctx, name); err == nil {
		rep.Interrupted = describe(op)
		if op.Hostname == s.Hostname && processAlive(op.PID) {
			rep.Interrupted = "in progress: " + rep.Interrupted
		}
	} else if !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	rep.Reconcile = &ReconcileStatus{GaveUp: s.repairs.gaveUp(name, s.Now())}
	if ops, err := s.Store.ListOperations(ctx, name, 0, 50); err == nil {
		for _, op := range ops {
			if op.Type == ReconcileOperation && op.Status != release.OperationRunning {
				rep.Reconcile.Last, rep.Reconcile.Status, rep.Reconcile.Error = op.FinishedAt, string(op.Status), op.Error
				break
			}
		}
	}
	applied, err := s.Store.ListResources(ctx, name)
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime(ctx)
	if err != nil {
		return nil, err
	}
	objs, err := rt.ListManaged(ctx, name)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, o := range objs {
		present[o.Kind+"/"+o.Name] = true
	}
	tracked := map[string]bool{}
	for _, a := range applied {
		st := ResourceStatus{Kind: a.Kind, Name: a.Name, RuntimeName: a.RuntimeName, Revision: a.Revision, Desired: "applied"}
		if a.Digest == "" {
			st.Desired = "failed"
		}
		switch a.Kind {
		case resource.KindWorkload, deployment.KindProxy:
			tracked[ports.ObjectWorkload+"/"+a.RuntimeName] = true
			ws, err := rt.GetWorkload(ctx, a.RuntimeName)
			switch {
			case errors.Is(err, ports.ErrNotFound):
				st.Actual = "missing"
			case err != nil:
				return nil, err
			default:
				st.Actual, st.Health, st.Restarts, st.OOMKilled = ws.State, ws.Health, ws.RestartCount, ws.OOMKilled
				if r, derr := resource.DecodeStored(a.Spec); a.Kind == resource.KindWorkload && derr == nil {
					if w, ok := r.Spec.(*resource.WorkloadSpec); ok {
						st.Health = s.Deploy.ProbeHealth(ctx, ws, w) // tcp/http checks are karga's
					}
				}
			}
			if a.Kind == deployment.KindProxy {
				st.Backend = s.backends(deployment.ProxyDir(s.Home, name), "")
			}
		case resource.KindService:
			// A Service has no container: it is routed when the release proxy sends its ports somewhere.
			st.Actual = "not routed"
			if b := s.backends(deployment.ProxyDir(s.Home, name), a.Name+"-p"); b != "" {
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

// backends lists the aliases a release proxy routes to, e.g. "karga-api-r2, karga-web-r3", for the
// clusters whose name starts with prefix.
func (s *Reads) backends(dir, prefix string) string {
	eps, _ := s.Deploy.Proxies.Endpoints(dir)
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

// Exec runs a command in a running workload of a release and returns its exit code.
func (s *Reads) Exec(ctx context.Context, rel, workload string, opts ports.ExecOptions) (int, error) {
	if _, err := s.Store.GetRelease(ctx, rel); err != nil {
		return -1, err
	}
	applied, err := s.Store.ListResources(ctx, rel)
	if err != nil {
		return -1, err
	}
	var names []string
	for _, a := range applied {
		if a.Kind != resource.KindWorkload {
			continue
		}
		if a.Name == workload {
			rt, err := s.Runtime(ctx)
			if err != nil {
				return -1, err
			}
			return rt.Exec(ctx, a.RuntimeName, opts)
		}
		names = append(names, a.Name)
	}
	return -1, fmt.Errorf("release %s has no workload %q (workloads: %s)", rel, workload, strings.Join(names, ", "))
}

// Diff compares revision from with revision to of a release (to 0: the current one).
func (s *Reads) Diff(ctx context.Context, name string, from, to int) (*RevisionDiff, error) {
	a, err := s.Store.GetRevision(ctx, name, from)
	if err != nil {
		return nil, err
	}
	b, err := s.revisionOrCurrent(ctx, name, to)
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
	return s.Store.ListResources(ctx, name)
}
