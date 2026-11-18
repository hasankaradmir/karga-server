package releases

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hasankaradmir/karga-server/internal/application/deployment/runtimeops"

	"github.com/hasankaradmir/karga-sdk/api"
	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/domain/release"

	"github.com/hasankaradmir/karga-server/internal/application/deployment"
	"github.com/hasankaradmir/karga-server/internal/application/deployment/gateway"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// List returns every release.
func (s *Service) List(ctx context.Context) ([]api.Release, error) {
	rels, err := s.Store.ListReleases(ctx)
	return wireAll(rels, wireRelease), err
}

// History returns a release's revisions, oldest first.
func (s *Service) History(ctx context.Context, name string) ([]api.Revision, error) {
	if _, err := s.Store.GetRelease(ctx, name); err != nil {
		return nil, err
	}
	revs, err := s.Store.ListRevisions(ctx, name)
	return wireAll(revs, wireRevision), err
}

// revisionOrCurrent resolves revision 0 to the current revision.
func (s *Service) revisionOrCurrent(ctx context.Context, name string, number int) (release.Revision, error) {
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
func (s *Service) GetValues(ctx context.Context, name string, revision int, all bool) (map[string]any, error) {
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
func (s *Service) GetNotes(ctx context.Context, name string, revision int) (string, error) {
	rev, err := s.revisionOrCurrent(ctx, name, revision)
	if err != nil {
		return "", err
	}
	return rev.Notes, nil
}

func (s *Service) GetManifest(ctx context.Context, name string, revision int) ([]resource.Resource, error) {
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

// ResourceStatus is shared with the CLI (karga-sdk/api).
type ResourceStatus = api.ResourceStatus

// StatusReport is shared with the CLI (karga-sdk/api).
type StatusReport = api.StatusReport

// ReconcileStatus is shared with the CLI (karga-sdk/api).
type ReconcileStatus = api.ReconcileStatus

// Status reports the release and the live state of each of its resources.
func (s *Service) Status(ctx context.Context, name string) (*StatusReport, error) {
	rel, err := s.Store.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{Release: wireRelease(rel), Resources: []ResourceStatus{}}
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
						st.Health = deployment.ProbeHealth(ctx, ws, w) // tcp/http checks are karga's
					}
				}
			}
			if a.Kind == deployment.KindProxy {
				st.Backend = backends(deployment.ProxyDir(s.Home, name), "")
			}
		case resource.KindService:
			// A Service has no container: it is routed when the release proxy sends its ports somewhere.
			st.Actual = "not routed"
			if b := backends(deployment.ProxyDir(s.Home, name), a.Name+"-p"); b != "" {
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
func backends(dir, prefix string) string {
	eps, _ := runtimeops.Proxies.Endpoints(dir)
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

// Gateway lists what the host gateway serves, across releases.
func (s *Service) Gateway(ctx context.Context) ([]gateway.Exposed, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	return gateway.Routes(ctx, rt, s.Home)
}

// GatewaySettings returns the host's gateway settings and, per listener, the services using it.
func (s *Service) GatewaySettings(ctx context.Context) (gateway.Settings, map[string][]string, error) {
	settings, err := gateway.ReadSettings(s.Home)
	if err != nil {
		return settings, nil, err
	}
	rt, err := s.Runtime(ctx)
	if err != nil {
		return settings, nil, err
	}
	users, err := gateway.Users(ctx, rt, s.Home)
	return settings, users, err
}

// GatewayHealth reports the gateway container and whether its Envoy is ready.
func (s *Service) GatewayHealth(ctx context.Context) (gateway.State, error) {
	rt, err := s.Runtime(ctx)
	if err != nil {
		return gateway.State{}, err
	}
	return gateway.Health(ctx, rt, s.Home)
}

// SetGatewayAccessLog stores the access log setting and applies it to a running gateway.
func (s *Service) SetGatewayAccessLog(ctx context.Context, on bool) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.AccessLog = on
		return nil
	})
}

// SetGatewayListen sets the host addresses every gateway listener binds.
func (s *Service) SetGatewayListen(ctx context.Context, addrs []string) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.Listen = addrs
		return nil
	})
}

// AddGatewayListener defines a listener services can expose ports on.
func (s *Service) AddGatewayListener(ctx context.Context, l gateway.Listener) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, _ map[string][]string) error {
		g.Listeners = append(g.Listeners, l)
		return nil
	})
}

// RemoveGatewayListener removes a listener no service uses.
func (s *Service) RemoveGatewayListener(ctx context.Context, name string) error {
	return s.updateGateway(ctx, func(g *gateway.Settings, users map[string][]string) error {
		i := slices.IndexFunc(g.Listeners, func(l gateway.Listener) bool { return l.Name == name })
		switch {
		case i < 0:
			return fmt.Errorf("gateway listener %q: %w", name, ports.ErrNotFound)
		case len(users[name]) > 0:
			return fmt.Errorf("gateway listener %q is used by %s; remove those exposures first", name, strings.Join(users[name], ", "))
		}
		g.Listeners = slices.Delete(g.Listeners, i, i+1)
		return nil
	})
}

// updateGateway changes the gateway settings under the gateway lock, stores them and applies them
// to a running gateway, waiting until it routes again.
func (s *Service) updateGateway(ctx context.Context, change func(*gateway.Settings, map[string][]string) error) error {
	rt, err := s.runtime(ctx)
	if err != nil {
		return err
	}
	err = func() error {
		unlock, err := gateway.Lock(s.Home)
		if err != nil {
			return err
		}
		defer unlock()
		settings, err := gateway.ReadSettings(s.Home)
		if err != nil {
			return err
		}
		users, err := gateway.Users(ctx, rt, s.Home)
		if err != nil {
			return err
		}
		if err := change(&settings, users); err != nil {
			return err
		}
		return gateway.WriteSettings(s.Home, settings)
	}()
	if err != nil {
		return err
	}
	if _, err := rt.GetWorkload(ctx, gateway.ContainerName); errors.Is(err, ports.ErrNotFound) {
		return nil // applied when a release next exposes something
	}
	targets, err := gateway.Sync(ctx, rt, runtimeops.Proxies, s.Home)
	if err != nil {
		return err
	}
	want := gateway.Want(targets, func(t gateway.Target) bool { return len(t.Routes) > 0 })
	if len(want) == 0 {
		return nil
	}
	return runtimeops.Proxies.WaitReady(ctx, gateway.Dir(s.Home), want, runtimeops.ProxyReadyTimeout)
}

// Exec runs a command in a running workload of a release and returns its exit code.
func (s *Service) Exec(ctx context.Context, rel, workload string, opts ports.ExecOptions) (int, error) {
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

// RevisionDiff is shared with the CLI (karga-sdk/api).
type RevisionDiff = api.RevisionDiff

// Diff compares revision from with revision to of a release (to 0: the current one).
func (s *Service) Diff(ctx context.Context, name string, from, to int) (*RevisionDiff, error) {
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
func (s *Service) Resources(ctx context.Context, name string) ([]api.AppliedResource, error) {
	rs, err := s.Store.ListResources(ctx, name)
	return wireAll(rs, wireApplied), err
}

// BackupState writes a consistent copy of the state database to w.
func (s *Service) BackupState(ctx context.Context, w io.Writer) error {
	b, ok := s.Store.(interface {
		Backup(ctx context.Context, dest string) error
	})
	if !ok {
		return errors.New("this state store cannot be backed up")
	}
	dir, err := os.MkdirTemp(s.Home, ".backup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	tmp := filepath.Join(dir, "karga.db")
	if err := b.Backup(ctx, tmp); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}
