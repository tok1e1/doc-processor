package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	LogLevel string

	HTTPAddr        string
	MetricsAddr     string
	ShutdownTimeout time.Duration

	PostgresDSN      string
	PostgresMaxConns int32

	RabbitURL string

	StorageDir string

	WorkerConcurrency int
	JobTimeout        time.Duration
	MaxAttempts       int
	RetryBaseDelay    time.Duration

	OutboxInterval  time.Duration
	OutboxBatchSize int
}

func Load() (Config, error) {
	var (
		c   Config
		err error
	)

	c.LogLevel = str("LOG_LEVEL", "info")
	c.HTTPAddr = str("HTTP_ADDR", ":8080")
	c.MetricsAddr = str("METRICS_ADDR", ":9090")
	c.PostgresDSN = str("POSTGRES_DSN", "postgres://docs:docs@localhost:5432/docs?sslmode=disable")
	c.RabbitURL = str("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	c.StorageDir = str("STORAGE_DIR", "./data/documents")

	if c.ShutdownTimeout, err = duration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.JobTimeout, err = duration("JOB_TIMEOUT", 30*time.Second); err != nil {
		return c, err
	}
	if c.RetryBaseDelay, err = duration("RETRY_BASE_DELAY", 2*time.Second); err != nil {
		return c, err
	}
	if c.OutboxInterval, err = duration("OUTBOX_INTERVAL", 500*time.Millisecond); err != nil {
		return c, err
	}

	maxConns, err := integer("POSTGRES_MAX_CONNS", 10)
	if err != nil {
		return c, err
	}
	c.PostgresMaxConns = int32(maxConns) //nolint:gosec // bounded by validation below
	if c.WorkerConcurrency, err = integer("WORKER_CONCURRENCY", 8); err != nil {
		return c, err
	}
	if c.MaxAttempts, err = integer("MAX_ATTEMPTS", 5); err != nil {
		return c, err
	}
	if c.OutboxBatchSize, err = integer("OUTBOX_BATCH_SIZE", 100); err != nil {
		return c, err
	}

	return c, c.validate()
}

func (c Config) validate() error {
	switch {
	case c.PostgresMaxConns < 1 || c.PostgresMaxConns > 1000:
		return fmt.Errorf("POSTGRES_MAX_CONNS must be in [1, 1000], got %d", c.PostgresMaxConns)
	case c.WorkerConcurrency < 1:
		return fmt.Errorf("WORKER_CONCURRENCY must be positive, got %d", c.WorkerConcurrency)
	case c.MaxAttempts < 1:
		return fmt.Errorf("MAX_ATTEMPTS must be positive, got %d", c.MaxAttempts)
	case c.OutboxBatchSize < 1:
		return fmt.Errorf("OUTBOX_BATCH_SIZE must be positive, got %d", c.OutboxBatchSize)
	}
	return nil
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func integer(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func duration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
