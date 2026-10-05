package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tok1e1/doc-processor/internal/outbox"
)

type OutboxRepository struct {
	pool *pgxpool.Pool
}

func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{pool: pool}
}

// ProcessBatch locks up to limit unpublished messages, hands them to publish and marks the
// successfully published ones. FOR UPDATE SKIP LOCKED lets several relays run in parallel
// without publishing the same row twice.
func (r *OutboxRepository) ProcessBatch(ctx context.Context, limit int, publish outbox.PublishFunc) (int, error) {
	var (
		published int
		pubErr    error
	)
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, routing_key, payload
			FROM outbox
			WHERE published_at IS NULL
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outbox.Message, error) {
			var m outbox.Message
			err := row.Scan(&m.ID, &m.RoutingKey, &m.Payload)
			return m, err
		})
		if err != nil || len(msgs) == 0 {
			return err
		}

		var ids []int64
		ids, pubErr = publish(ctx, msgs)
		if len(ids) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
				return err
			}
		}
		published = len(ids)
		// Commit what was published even if the rest of the batch failed:
		// the remaining rows stay unpublished and will be retried.
		return nil
	})
	if err != nil {
		return published, err
	}
	return published, pubErr
}

// Cleanup deletes messages published before the given moment.
func (r *OutboxRepository) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM outbox WHERE published_at < $1`, before)
	return tag.RowsAffected(), err
}
