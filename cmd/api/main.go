// Command api accepts document generation requests and serves the results.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tok1e1/doc-processor/internal/app"
	"github.com/tok1e1/doc-processor/internal/blob"
	"github.com/tok1e1/doc-processor/internal/config"
	"github.com/tok1e1/doc-processor/internal/metrics"
	"github.com/tok1e1/doc-processor/internal/outbox"
	"github.com/tok1e1/doc-processor/internal/queue/rabbitmq"
	"github.com/tok1e1/doc-processor/internal/render"
	"github.com/tok1e1/doc-processor/internal/service"
	"github.com/tok1e1/doc-processor/internal/storage/postgres"
	"github.com/tok1e1/doc-processor/internal/transport/httpapi"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := app.NewLogger(cfg.LogLevel, "api")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg, log)
	stop()
	if err != nil {
		log.Error("api stopped with error", "err", err)
		os.Exit(1)
	}
	log.Info("api stopped")
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := postgres.Connect(ctx, cfg.PostgresDSN, cfg.PostgresMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	templates, err := render.Default()
	if err != nil {
		return err
	}
	documents, err := blob.NewFS(cfg.StorageDir)
	if err != nil {
		return err
	}

	retry := rabbitmq.RetryPolicy{BaseDelay: cfg.RetryBaseDelay, MaxDelay: time.Minute, MaxAttempts: cfg.MaxAttempts}
	publisher := rabbitmq.NewPublisher(cfg.RabbitURL, retry, log)
	defer publisher.Close()

	m := metrics.New()

	jobs := service.NewJobs(postgres.NewJobRepository(pool), templates, documents, rabbitmq.RoutingKeyJobs)
	jobs.OnCreated = func(t string) { m.JobsCreated.WithLabelValues(t).Inc() }

	relay := outbox.NewRelay(postgres.NewOutboxRepository(pool), publisher, log)
	relay.Interval = cfg.OutboxInterval
	relay.BatchSize = cfg.OutboxBatchSize
	relay.OnPublished = func(n int) { m.OutboxPublished.Add(float64(n)) }

	handler := httpapi.NewHandler(jobs, templates.Names(), map[string]httpapi.Checker{
		"postgres": pool,
		"rabbitmq": publisher,
	}, log)

	apiServer := app.NewServer(cfg.HTTPAddr, httpapi.NewRouter(handler, m, log))
	metricsServer := app.NewServer(cfg.MetricsAddr, m.Handler())

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("http server started", "addr", cfg.HTTPAddr)
		return app.Serve(ctx, apiServer, cfg.ShutdownTimeout)
	})
	g.Go(func() error {
		return app.Serve(ctx, metricsServer, cfg.ShutdownTimeout)
	})
	g.Go(func() error {
		relay.Run(ctx)
		return nil
	})
	return g.Wait()
}
