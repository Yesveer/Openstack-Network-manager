package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yesveer/openstack-net-attach/internal/config"
	"github.com/yesveer/openstack-net-attach/internal/daemon"
	"github.com/yesveer/openstack-net-attach/internal/network"
)

// version is injected at build time via -ldflags.
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/openstack-net-attach/config.yaml", "path to config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("openstack-net-attach " + version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	logger.Info("openstack-net-attach starting", "version", version)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	logger.Info("configuration loaded",
		"backend", cfg.Backend,
		"include_prefixes", cfg.IncludePrefixes,
		"exclude_prefixes", cfg.ExcludePrefixes,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	}()

	mgr := network.NewManager(cfg, logger)
	d := daemon.New(mgr, logger)

	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}

	logger.Info("openstack-net-attach stopped")
}
