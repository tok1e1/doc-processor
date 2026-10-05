// Command worker consumes generation jobs from RabbitMQ and renders PDF documents.
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
	"github.com/tok1e1/doc-processor/internal/queue"
	"github.com/tok1e1/doc-processor/internal/queue/rabbitmq"
	"github.com/tok1e1/doc-processor/internal/render"
	"github.com/tok1e1/doc-processor/internal/storage/postgres"
	"github.com/tok1e1/doc-processor/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := app.NewLogger(cfg.LogLevel, "worker")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg, log)
	stop()
	if err != nil {
		log.Error("worker stopped with error", "err", err)
		os.Exit(1)
	}
	log.Info("worker stopped")
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
	consumer := rabbitmq.NewConsumer(cfg.RabbitURL, cfg.WorkerConcurrency, retry, log)

	m := metrics.New()
	processor := worker.NewProcessor(postgres.NewJobRepository(pool), templates, documents, publisher,
		metricsObserver{m}, log, worker.Options{
			MaxAttempts:  cfg.MaxAttempts,
			JobTimeout:   cfg.JobTimeout,
			RequeueDelay: time.Second,
		})

	workers := worker.NewPool(cfg.WorkerConcurrency, processor.Handle)
	workers.OnBusyChange = m.WorkersBusy.Add
	workers.OnPanic = func(v any) { log.Error("handler panic, message rejected", "value", v) }

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return app.Serve(gctx, app.NewServer(cfg.MetricsAddr, m.Handler()), cfg.ShutdownTimeout)
	})
	g.Go(func() error {
		consume(gctx, consumer, workers, cfg.ShutdownTimeout, log)
		return nil
	})
	return g.Wait()
}

// consume runs the consumer and the pool until ctx is cancelled, then gives in-flight
// messages up to timeout to finish. Anything left unacknowledged is redelivered by RabbitMQ.
func consume(ctx context.Context, c *rabbitmq.Consumer, workers *worker.Pool, timeout time.Duration, log *slog.Logger) {
	msgs := make(chan queue.Message)
	go c.Run(ctx, msgs)

	done := make(chan struct{})
	go func() {
		workers.Run(context.WithoutCancel(ctx), msgs)
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		log.Info("shutting down, waiting for in-flight jobs", "timeout", timeout)
		select {
		case <-done:
		case <-time.After(timeout):
			log.Warn("shutdown timeout exceeded, unfinished jobs will be redelivered")
		}
	}
	if err := c.Close(); err != nil {
		log.Warn("close consumer", "err", err)
	}
}

type metricsObserver struct {
	m *metrics.Metrics
}

func (o metricsObserver) Processed(template, outcome string, renderTime time.Duration) {
	o.m.JobsProcessed.WithLabelValues(template, outcome).Inc()
	if outcome == worker.OutcomeDone {
		o.m.RenderDuration.WithLabelValues(template).Observe(renderTime.Seconds())
	}
}
