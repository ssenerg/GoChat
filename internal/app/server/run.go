// Package server runs the chat server: the actors, their stores, and the
// DRPC API the web gateways call.
package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/repos/minio"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/server/admin"
	"github.com/ssenerg/gochat/internal/server/drpc"
	"github.com/ssenerg/gochat/internal/services"
	"github.com/ssenerg/gochat/internal/utils/logging"
)

const (
	actorsStopTimeout = 15 * time.Second
	shutdownGrace     = 15 * time.Second
	// How long the server waits for Scylla to accept connections. When the
	// machine boots, they start together, and exiting at once would only have
	// Docker restart the server again and again until Scylla is up.
	scyllaStartTimeout = 2 * time.Minute
)

func Run(parent context.Context, cfg *Config) error {
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()
	mainLogger := logger.Named("main")

	scyllaCtx, scyllaCancel := context.WithTimeout(ctx, scyllaStartTimeout)
	repo, err := scylla.NewRepository(scyllaCtx, logger.Named("scylla"), cfg.Database.Scylla)
	scyllaCancel()
	if err != nil {
		return err
	}
	defer repo.Stop()

	startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
	storage, err := minio.New(startCtx, logger.Named("minio"), cfg.Storage.Minio)
	startCancel()
	if err != nil {
		mainLogger.Error("failed to open object storage", zap.Error(err))
		return err
	}

	az, err := authz.New(logger.Named("authz"), repo.CasbinAdapter(10*time.Second))
	if err != nil {
		mainLogger.Error("failed to load authorization policy", zap.Error(err))
		return err
	}

	sys, err := services.Start(logger, cfg.Services, repo, storage, az)
	if err != nil {
		mainLogger.Error("failed to start actors", zap.Error(err))
		return err
	}

	var (
		api   *drpc.Server
		probe *admin.Server
	)
	defer func() {
		stop := shutdownWatchdog(mainLogger, cfg.Server.DRPC.ShutdownTimeout+actorsStopTimeout+shutdownGrace)
		defer stop()
		api.Stop()
		sys.Stop(actorsStopTimeout)
		probe.Stop()
	}()

	mux, err := drpc.Mux(sys, storage, cfg.Services)
	if err != nil {
		return err
	}
	api = drpc.New(logger, cfg.Server.DRPC, mux, drpc.Guard(logger, cfg.Server.DRPC, drpc.Resolve(sys)))
	if err := api.Start(); err != nil {
		mainLogger.Error("failed to start drpc server", zap.Error(err))
		return err
	}

	probe = admin.New(logger, cfg.Server.Admin, prometheus.NewRegistry(),
		admin.Check{Name: "scylla", Ready: repo.Ping},
		admin.Check{Name: "minio", Ready: storage.Ping},
	)
	if err := probe.Start(); err != nil {
		mainLogger.Error("failed to start admin server", zap.Error(err))
		return err
	}

	mainLogger.Info("started")
	select {
	case <-ctx.Done():
	case err = <-api.Failures():
		mainLogger.Error("drpc server failed", zap.Error(err))
	case err = <-probe.Failures():
		mainLogger.Error("admin server failed", zap.Error(err))
	}
	if err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

func shutdownWatchdog(logger *zap.Logger, d time.Duration) (stop func() bool) {
	timer := time.AfterFunc(d, func() {
		logger.Error("shutdown timed out, forcing exit", zap.Duration("after", d))
		os.Exit(1)
	})
	return timer.Stop
}
