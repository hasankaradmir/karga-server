package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hasankaradmir/karga-sdk/resource"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
	"github.com/hasankaradmir/karga-server/internal/ports"
)

// validateTimeout bounds Envoy's check of a configuration.
const validateTimeout = 30 * time.Second

// validateEnvoy has Envoy check the configuration settings would give the gateway, in a container
// of its own that serves nothing (D40): what Envoy refuses is found before the gateway is touched.
// It returns Envoy's reason, empty when it accepts the configuration. Addresses and ports are not
// bound by the check: those failures still show on apply.
func (g *Gateway) validateEnvoy(ctx context.Context, settings Settings) (string, error) {
	rt, h, home := g.rt, g.host.Proxies, g.home
	c := h.Container()
	targets, err := findTargets(ctx, rt, home)
	if err != nil {
		return "", err
	}
	cfg := buildConfig(settings, targets, c.MountPath)
	cfg.AccessLog = settings.AccessLog
	boot, err := h.ValidationBootstrap(cfg)
	if err != nil {
		return err.Error(), nil
	}
	var id [4]byte
	_, _ = rand.Read(id[:])
	name := ContainerName + "-validate-" + hex.EncodeToString(id[:])
	dir := filepath.Join(Dir(home), "validate-"+hex.EncodeToString(id[:]))
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeFile(filepath.Join(dir, "envoy.yaml"), boot); err != nil {
		return "", err
	}
	if err := syncCerts(dir, targets, false); err != nil {
		return "", err
	}
	if err := rt.EnsureImage(ctx, c.Image, "if-not-present", nil); err != nil {
		return "", err
	}
	wr := ports.WorkloadRequest{
		Name:    name,
		Labels:  map[string]string{ports.LabelManaged: "true", LabelComponent: "gateway-validate"},
		Image:   c.Image,
		Command: h.ValidateCommand(c.MountPath + "/envoy.yaml"),
		// Root with DAC_OVERRIDE, like the gateway, reads the 0600 files of karga-server's user.
		User:            "0:0",
		Env:             map[string]string{"ENVOY_UID": "0"},
		CapDrop:         []string{"ALL"},
		CapAdd:          []string{"DAC_OVERRIDE"},
		NoNewPrivileges: true,
		RestartPolicy:   "never",
		Mounts:          []ports.Mount{{Type: ports.MountBind, Source: dir, Target: c.MountPath, ReadOnly: true}},
		MemoryBytes:     c.MemoryBytes,
		MilliCPU:        c.MilliCPU,
		PidsLimit:       c.Pids,
		LogMaxSize:      resource.DefaultLogMaxSize,
		LogMaxFiles:     resource.DefaultLogMaxFiles,
	}
	if err := rt.CreateWorkload(ctx, wr); err != nil {
		return "", err
	}
	defer func() { _ = proxyops.RemoveWorkload(context.WithoutCancel(ctx), rt, name) }()
	if err := rt.StartWorkload(ctx, name); err != nil {
		return "", err
	}
	deadline := time.Now().Add(validateTimeout)
	for {
		st, err := rt.GetWorkload(ctx, name)
		if err != nil {
			return "", err
		}
		if st.State == ports.StateExited {
			if st.ExitCode == 0 {
				return "", nil
			}
			return envoyReason(ctx, rt, name, st.ExitCode), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("envoy did not finish checking the gateway configuration within %s", validateTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// envoyReason is the last lines Envoy logged when it refused a configuration.
func envoyReason(ctx context.Context, rt Runtime, name string, code int) string {
	var out strings.Builder
	if err := rt.Logs(ctx, name, 20, false, &out); err != nil && !errors.Is(err, ports.ErrNotFound) {
		return fmt.Sprintf("envoy refused the configuration (exit %d)", code)
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("envoy refused the configuration (exit %d)", code)
	}
	return "envoy: " + strings.Join(lines[max(0, len(lines)-3):], "; ")
}
