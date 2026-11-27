package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/hasankaradmir/karga-server/internal/ports"
)

// A permission error of a container (an entrypoint that is not executable, a bind mount) keeps its
// sentinel but gets no hint about the Docker socket: Docker's own message says what was refused.
func TestWrapPermissionDenied(t *testing.T) {
	cause := errors.New(`Error response from daemon: failed to create task: exec: "/entrypoint.sh": permission denied`)
	err := wrap(cause, "start workload web")
	if !errors.Is(err, ports.ErrPermission) {
		t.Fatalf("not ErrPermission: %v", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) || strings.Contains(err.Error(), "Docker socket") {
		t.Fatalf("message: %v", err)
	}
}
