package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sratabix/dell-ipmitemps/internal/config"
	"github.com/sratabix/dell-ipmitemps/internal/controller"
)

var version = "dev"

const defaultConfigPath = "/etc/dellipmifanctl/config.yaml"

func main() {
	configPath := flag.String("config", configPathFromEnv(), "path to the YAML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	logger.Info("dellipmifanctl starting",
		"version", version,
		"poll_interval_s", cfg.PollInterval,
		"sensors", len(cfg.Sensors))

	return controller.New(cfg, logger).Run(ctx)
}

func configPathFromEnv() string {
	if p := os.Getenv("DELLIPMIFANCTL_CONFIG"); p != "" {
		return p
	}
	return defaultConfigPath
}
