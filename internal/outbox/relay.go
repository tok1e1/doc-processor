// Package outbox moves messages from the outbox table to the broker.
package outbox

import (
	"context"
	"log/slog"
	"time"
)

type Message struct {
	ID         int64
	RoutingKey string
	Payload    []byte
}

// PublishFunc publishes messages and returns the IDs that the broker confirmed.
type PublishFunc func(ctx context.Context, msgs []Message) ([]int64, error)

type Store interface {
	ProcessBatch(ctx context.Context, limit int, publish PublishFunc) (int, error)
	Cleanup(ctx context.Context, before time.Time) (int64, error)
}

type Publisher interface {
	Publish(ctx context.Context, routingKey string, body []byte) error
}

type Relay struct {
	store     Store
	publisher Publisher
	log       *slog.Logger

	Interval  time.Duration
	BatchSize int
	// Retention defines how long published messages are kept before cleanup.
	Retention time.Duration

	OnPublished func(n int)
}

func NewRelay(store Store, publisher Publisher, log *slog.Logger) *Relay {
	return &Relay{
		store:     store,
		publisher: publisher,
		log:       log,
		Interval:  500 * time.Millisecond,
		BatchSize: 100,
		Retention: 24 * time.Hour,
	}
}

// Run polls the outbox until ctx is cancelled. A full batch triggers the next poll immediately
// so a backlog drains without waiting for the ticker.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()

	for {
		n, err := r.store.ProcessBatch(ctx, r.BatchSize, r.publish)
		if err != nil && ctx.Err() == nil {
			r.log.Error("outbox relay", "err", err, "published", n)
		}
		if n > 0 && r.OnPublished != nil {
			r.OnPublished(n)
		}
		if n == r.BatchSize && err == nil {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-cleanup.C:
			deleted, err := r.store.Cleanup(ctx, time.Now().Add(-r.Retention))
			if err != nil {
				r.log.Error("outbox cleanup", "err", err)
			} else if deleted > 0 {
				r.log.Info("outbox cleanup", "deleted", deleted)
			}
		}
	}
}

func (r *Relay) publish(ctx context.Context, msgs []Message) ([]int64, error) {
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		// Stop on the first error to preserve ordering; the rest is retried on the next tick.
		if err := r.publisher.Publish(ctx, m.RoutingKey, m.Payload); err != nil {
			return ids, err
		}
		ids = append(ids, m.ID)
	}
	return ids, nil
}
