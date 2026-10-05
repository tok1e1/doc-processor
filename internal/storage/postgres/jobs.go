package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tok1e1/doc-processor/internal/domain"
)

const jobColumns = `id, template, payload, status, attempts, error, document_key,
	COALESCE(idempotency_key, ''), created_at, updated_at, finished_at`

type JobRepository struct {
	pool *pgxpool.Pool
}

func NewJobRepository(pool *pgxpool.Pool) *JobRepository {
	return &JobRepository{pool: pool}
}

// Create stores the job and an outbox record in one transaction (transactional outbox),
// so a job is never persisted without the message that triggers its processing.
//
// When a job with the same idempotency key already exists, it is returned with created=false.
func (r *JobRepository) Create(ctx context.Context, job domain.Job, routingKey string) (_ domain.Job, created bool, err error) {
	msg, err := json.Marshal(domain.GenerateMessage{JobID: job.ID})
	if err != nil {
		return domain.Job{}, false, err
	}

	var idemKey *string
	if job.IdempotencyKey != "" {
		idemKey = &job.IdempotencyKey
	}

	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO jobs (id, template, payload, status, idempotency_key)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
			RETURNING `+jobColumns,
			job.ID, job.Template, job.Payload, domain.StatusPending, idemKey)

		job, err = scanJob(row)
		if errors.Is(err, domain.ErrNotFound) {
			row = tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE idempotency_key = $1`, idemKey)
			job, err = scanJob(row)
			return err
		}
		if err != nil {
			return err
		}

		created = true
		_, err = tx.Exec(ctx, `INSERT INTO outbox (routing_key, payload) VALUES ($1, $2)`, routingKey, msg)
		return err
	})
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("create job: %w", err)
	}
	return job, created, nil
}

func (r *JobRepository) Get(ctx context.Context, id uuid.UUID) (domain.Job, error) {
	return scanJob(r.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
}

// StartProcessing moves the job to "processing" and bumps the attempt counter.
// Jobs that are already finished are returned unchanged so the caller can skip duplicates.
func (r *JobRepository) StartProcessing(ctx context.Context, id uuid.UUID) (domain.Job, error) {
	job, err := scanJob(r.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = $2, attempts = attempts + 1, updated_at = now()
		WHERE id = $1 AND status IN ('pending', 'processing')
		RETURNING `+jobColumns, id, domain.StatusProcessing))
	if errors.Is(err, domain.ErrNotFound) {
		return r.Get(ctx, id)
	}
	return job, err
}

func (r *JobRepository) MarkDone(ctx context.Context, id uuid.UUID, documentKey string) error {
	return r.exec(ctx, `
		UPDATE jobs
		SET status = $2, document_key = $3, error = '', updated_at = now(), finished_at = now()
		WHERE id = $1`, id, domain.StatusDone, documentKey)
}

func (r *JobRepository) MarkFailed(ctx context.Context, id uuid.UUID, reason string) error {
	return r.exec(ctx, `
		UPDATE jobs
		SET status = $2, error = $3, updated_at = now(), finished_at = now()
		WHERE id = $1`, id, domain.StatusFailed, reason)
}

// MarkRetry returns the job to "pending" and records the last error.
func (r *JobRepository) MarkRetry(ctx context.Context, id uuid.UUID, reason string) error {
	return r.exec(ctx, `
		UPDATE jobs
		SET status = $2, error = $3, updated_at = now()
		WHERE id = $1`, id, domain.StatusPending, reason)
}

func (r *JobRepository) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func scanJob(row pgx.Row) (domain.Job, error) {
	var (
		j      domain.Job
		status string
	)
	err := row.Scan(&j.ID, &j.Template, &j.Payload, &status, &j.Attempts, &j.Error,
		&j.DocumentKey, &j.IdempotencyKey, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Job{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Job{}, err
	}
	j.Status = domain.Status(status)
	return j, nil
}
