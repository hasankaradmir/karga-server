// Command karga-server owns karga's state and container runtime and serves the karga CLI on a
// local unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga-server/internal/infrastructure/logging"

	"github.com/hasankaradmir/karga-sdk/api"

	"github.com/hasankaradmir/karga-server/internal/adapters/docker"
	"github.com/hasankaradmir/karga-server/internal/infrastructure/config"
	"github.com/hasankaradmir/karga-server/internal/server"
	"github.com/hasankaradmir/karga-server/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "karga-server:", err)
		os.Exit(1)
	}
}

func run() error {
	home := flag.String("home", "", "state directory (default $KARGA_HOME, $XDG_DATA_HOME/karga or ~/.local/share/karga)")
	socket := flag.String("socket", envOr("KARGA_SOCKET", api.DefaultSocket), "unix socket the CLI connects to")
	interval := flag.Duration("reconcile-interval", server.DefaultReconcileInterval, "time between reconciliations of every release (0: only on container events)")
	allowCaps := flag.String("allow-capabilities", "", "comma-separated Linux capabilities packages may add beyond Docker's default set, e.g. NET_ADMIN (ALL: any; each one gives containers power over the host)")
	grace := flag.Duration("shutdown-grace", 5*time.Minute, "how long running operations may take to finish when the server stops (keep the systemd unit's TimeoutStopSec above it)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		v := version.Current()
		fmt.Printf("karga-server %s (commit %s, %s)\n", v.Version, v.Commit, v.GoVersion)
		return nil
	}
	cfg, err := config.Resolve(*home, os.Getenv)
	if err != nil {
		return err
	}
	redactor := logging.NewRedactor()
	log, err := logging.New(os.Stderr, "text", slog.LevelInfo, redactor)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rt, err := docker.New(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	srv, err := server.New(ctx, server.Config{Home: cfg.Home, Socket: *socket, Log: log, Redactor: redactor, AllowedCaps: splitList(*allowCaps), ReconcileInterval: *interval}, rt)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	l, err := srv.Listen()
	if err != nil {
		return err
	}
	return srv.Serve(ctx, l, *grace)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
