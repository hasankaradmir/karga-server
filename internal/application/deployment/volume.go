package deployment

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// WithProgress sends the progress events of what runs with the returned context to fn.
func WithProgress(ctx context.Context, fn func(Event)) context.Context { return withProgress(ctx, fn) }

// WithDependents returns the workloads among names and those that depend on them through
// dependsOn (one of their Services), at any depth, in start order: dependencies first. Stopping
// them goes the other way.
func WithDependents(rs []resource.Resource, names []string) ([]string, error) {
	ordered, err := Order(rs)
	if err != nil {
		return nil, err
	}
	target := map[string]string{} // Service -> its workload
	for _, r := range rs {
		if s, ok := r.Spec.(*resource.ServiceSpec); ok {
			target[r.Metadata.Name] = s.Workload
		}
	}
	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}
	for grew := true; grew; {
		grew = false
		for _, r := range rs {
			w, ok := r.Spec.(*resource.WorkloadSpec)
			if !ok || in[r.Metadata.Name] {
				continue
			}
			if slices.ContainsFunc(w.DependsOn, func(s string) bool { return in[target[s]] }) {
				in[r.Metadata.Name], grew = true, true
			}
		}
	}
	var out []string
	for _, r := range ordered {
		if r.Kind == resource.KindWorkload && in[r.Metadata.Name] {
			out = append(out, r.Metadata.Name)
		}
	}
	return out, nil
}

// WaitReady waits until each container (name -> its workload's spec) is ready, as a deployment
// waits: Docker's health for exec checks, karga's probes for tcp and http, a crash loop or the
// timeout (DefaultTimeout when 0) fails it.
func (e *Env) WaitReady(ctx context.Context, rt ports.Runtime, containers map[string]*resource.WorkloadSpec, timeout time.Duration) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	var ts []readyTarget
	for _, n := range slices.Sorted(maps.Keys(containers)) {
		ts = append(ts, readyTarget{n, containers[n]})
	}
	_, err := e.waitReady(ctx, rt, ts, timeout)
	return err
}

// Mounter is an applied workload that mounts a volume, as RecreateVolume creates it again.
type Mounter struct {
	Name, Container, Digest string
	Revision                int
	Spec                    *resource.WorkloadSpec
	// Paths are the materialized Config and Secret files of its revision.
	Paths map[Key]string
}

// RecreateVolume empties a volume. It removes the containers that mount it (Docker keeps a volume
// a container mounts, even a stopped one), removes the volume and creates it again with the same
// request, then creates the containers again as they were: same name, labels, revision and
// anonymous volumes, so the release proxy finds them under the same alias. They are not started:
// the caller fills the volume through one of them first. Run again after an interruption, it
// finishes the job.
func (e *Env) RecreateVolume(ctx context.Context, rt ports.Runtime, release string, vol ports.VolumeRequest, mounters []Mounter) error {
	reqs := make([]ports.WorkloadRequest, len(mounters))
	for i, m := range mounters {
		lbl := labels(release, m.Revision, resource.KindWorkload, m.Name)
		lbl[ports.LabelDigest] = m.Digest
		wr, err := workloadRequest(release, m.Name, m.Container, m.Spec, lbl, m.Paths)
		if err != nil {
			return err
		}
		if wr.Mounts, err = anonymousMounts(ctx, rt, m.Container, wr.Mounts); err != nil {
			return err
		}
		reqs[i] = wr
	}
	for i, wr := range reqs {
		progress(ctx, resource.KindWorkload, mounters[i].Name, PhaseRemove, "removing %s", wr.Name)
		if err := rt.DeleteWorkload(ctx, wr.Name); err != nil { // keeps its anonymous volumes
			return err
		}
	}
	progress(ctx, resource.KindVolume, vol.Labels[ports.LabelName], PhaseCreate, "creating %s again, empty", vol.Name)
	if err := rt.DeleteVolume(ctx, vol.Name); err != nil {
		return fmt.Errorf("remove volume %s: %w", vol.Name, err)
	}
	if err := rt.CreateVolume(ctx, vol); err != nil {
		return fmt.Errorf("create volume %s: %w", vol.Name, err)
	}
	for i, wr := range reqs {
		progress(ctx, resource.KindWorkload, mounters[i].Name, PhaseCreate, "creating %s", wr.Name)
		if err := rt.CreateWorkload(ctx, wr); err != nil {
			return err
		}
	}
	return nil
}
