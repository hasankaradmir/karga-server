package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// The gateway reaches the release proxies on the edge network (D42): one bridge every proxy joins
// at a fixed address, with traffic between its containers off (releases stay apart) and no way
// out (internal); the host, so the gateway in its network, reaches every address on it.
const (
	EdgeNetwork = "karga-edge"
	// LabelEdgeIP on a release proxy container is its address on the edge network.
	LabelEdgeIP = "io.karga.edge-ip"
)

// EdgeRuntime is what the edge network needs of a container runtime.
type EdgeRuntime interface {
	CreateNetwork(ctx context.Context, req ports.NetworkRequest) error
	ListLabeled(ctx context.Context, key string) ([]ports.Object, error)
}

// EnsureEdge creates the edge network with subnet unless it exists. A network made with another
// subnet shows when a proxy is connected at an address outside it.
func EnsureEdge(ctx context.Context, rt EdgeRuntime, subnet string) error {
	err := rt.CreateNetwork(ctx, ports.NetworkRequest{
		Name: EdgeNetwork, Driver: "bridge", Internal: true, Subnet: subnet,
		Labels:  map[string]string{ports.LabelManaged: "true", LabelComponent: "edge"},
		Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"},
	})
	if errors.Is(err, ports.ErrConflict) {
		return nil
	}
	return err
}

// edgeState is $KARGA_HOME/edge.yaml: each release's pair of addresses, and where the next pair
// is looked for.
type edgeState struct {
	Next     uint32              `yaml:"next"`
	Releases map[string][]string `yaml:"releases,omitempty"`
}

func edgePath(home string) string { return filepath.Join(home, "edge.yaml") }

func readEdge(home string) (edgeState, error) {
	var s edgeState
	data, err := os.ReadFile(edgePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("%s: %w", edgePath(home), err)
	}
	return s, nil
}

func writeEdge(home string, s edgeState) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return writeFile(edgePath(home), data)
}

// EdgeIP returns the edge address for a new proxy of release rel: of the release's pair, the one
// avoid (the address of the proxy it replaces, if any) does not hold. A release gets its pair the
// first time: the next two free addresses after the last pair given, so an address left by a
// release is not given again for long.
func EdgeIP(ctx context.Context, rt EdgeRuntime, home, subnet, rel, avoid string) (string, error) {
	unlock, err := Lock(ctx, home)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := readEdge(home)
	if err != nil {
		return "", err
	}
	pair := s.Releases[rel]
	if len(pair) != 2 {
		if pair, err = allocatePair(ctx, rt, &s, subnet); err != nil {
			return "", err
		}
		if s.Releases == nil {
			s.Releases = map[string][]string{}
		}
		s.Releases[rel] = pair
		if err := writeEdge(home, s); err != nil {
			return "", err
		}
	}
	if pair[0] == avoid {
		return pair[1], nil
	}
	return pair[0], nil
}

// ReleaseEdge forgets the pair of a release that has no proxy any more.
func ReleaseEdge(ctx context.Context, home, rel string) error {
	unlock, err := Lock(ctx, home)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := readEdge(home)
	if err != nil || s.Releases[rel] == nil {
		return err
	}
	delete(s.Releases, rel)
	return writeEdge(home, s)
}

// allocatePair takes the next two free addresses of subnet from s.Next on (wrapping), leaving out
// the network address, Docker's gateway (the first), the broadcast address, the pairs given and
// the addresses proxies hold.
func allocatePair(ctx context.Context, rt EdgeRuntime, s *edgeState, subnet string) ([]string, error) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, p := range s.Releases {
		for _, ip := range p {
			used[ip] = true
		}
	}
	objs, err := rt.ListLabeled(ctx, LabelEdgeIP)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		used[o.Labels[LabelEdgeIP]] = true
	}
	base := binary.BigEndian.Uint32(prefix.Masked().Addr().AsSlice())
	size := uint32(1) << (32 - prefix.Bits())
	first, last := uint32(2), size-2 // offsets: .0 is the network, .1 Docker's gateway, size-1 broadcast
	count := last - first + 1
	var pair []string
	start := s.Next
	for i := uint32(0); i < count && len(pair) < 2; i++ {
		n := (start + i) % count
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+first+n)
		if ip := netip.AddrFrom4(b).String(); !used[ip] {
			pair = append(pair, ip)
			s.Next = (n + 1) % count
		}
	}
	if len(pair) < 2 {
		return nil, fmt.Errorf("edge network %s: no free addresses left: %w", subnet, ports.ErrConflict)
	}
	return pair, nil
}
