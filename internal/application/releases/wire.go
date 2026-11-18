package releases

import (
	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-server/internal/application/gateway"

	"github.com/hasankaradmir/karga-server/internal/domain/release"
)

// The state model mapped to what the CLI sees.

func wireRelease(r release.Release) api.Release {
	return api.Release{Name: r.Name, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		CurrentRevision: r.CurrentRevision, Status: string(r.Status), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Paused: r.Paused}
}

func wireRevision(r release.Revision) api.Revision {
	return api.Revision{Release: r.Release, Number: r.Number, PackageName: r.PackageName, PackageVersion: r.PackageVersion,
		PackageDigest: r.PackageDigest, ValuesDigest: r.ValuesDigest, ManifestDigest: r.ManifestDigest,
		Status: string(r.Status), Description: r.Description, CreatedAt: r.CreatedAt}
}

func wireOperation(o release.Operation) api.Operation {
	return api.Operation{ID: o.ID, Release: o.Release, Type: o.Type, Revision: o.Revision, Status: string(o.Status),
		Actor: o.Actor, Steps: wireSteps(o.Steps), Error: o.Error, StartedAt: o.StartedAt, FinishedAt: o.FinishedAt}
}

func wireStep(s release.Step) api.Step {
	return api.Step{Kind: s.Kind, Name: s.Name, Action: s.Action, Status: s.Status, Error: s.Error}
}

func wireSteps(ss []release.Step) []api.Step {
	return wireAll(ss, wireStep)
}

func wireApplied(a release.AppliedResource) api.AppliedResource {
	return api.AppliedResource{Kind: a.Kind, Name: a.Name, Revision: a.Revision, RuntimeName: a.RuntimeName}
}

// wireAll maps a list; nil stays nil so empty JSON lists keep their meaning.
func wireAll[T, W any](in []T, f func(T) W) []W {
	if in == nil {
		return nil
	}
	out := make([]W, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func wireSettings(g gateway.Settings) api.Settings {
	out := api.Settings{Listen: g.Listen, AccessLog: g.AccessLog, Listeners: []api.Listener{}}
	for _, l := range g.Listeners {
		out.Listeners = append(out.Listeners, api.Listener{Name: l.Name, Port: l.Port, Protocol: l.Protocol})
	}
	return out
}
