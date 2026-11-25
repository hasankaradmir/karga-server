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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/hasankaradmir/karga-server/internal/application/proxyops"
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
	edge := flag.String("edge-subnet", envOr("KARGA_EDGE_SUBNET", proxyops.DefaultEdgeSubnet), "address range of the network the gateway reaches release proxies on (IPv4, /16 to /28; it cannot change once proxies use it)")
	logFormat := flag.String("log-format", "text", "log format: text or json")
	logLevel := flag.String("log-level", "info", "lowest level logged: debug, info, warn or error")
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
	if err := proxyops.ValidateEdgeSubnet(*edge); err != nil {
		return err
	}
	redactor := logging.NewRedactor()
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	log, err := logging.New(os.Stderr, *logFormat, level, redactor)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// A second signal does not wait for running operations: they resume at the next start.
		again := make(chan os.Signal, 1)
		signal.Notify(again, os.Interrupt, syscall.SIGTERM)
		<-again
		log.Warn("second signal: exiting without waiting for running operations")
		os.Exit(1)
	}()

	rt, err := docker.New(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	caps := splitList(*allowCaps)
	if slices.ContainsFunc(caps, func(c string) bool { return strings.EqualFold(c, "ALL") }) {
		log.Warn("--allow-capabilities ALL: packages may give their containers any capability, power over the host included")
	}
	srv, err := server.New(ctx, server.Config{Home: cfg.Home, Socket: *socket, Log: log, Redactor: redactor, AllowedCaps: caps, ReconcileInterval: *interval, EdgeSubnet: *edge}, rt)
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
