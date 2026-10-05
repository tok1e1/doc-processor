// Package worker renders documents for jobs received from the broker.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
	"github.com/tok1e1/doc-processor/internal/queue"
)

type Repository interface {
	StartProcessing(ctx context.Context, id uuid.UUID) (domain.Job, error)
	MarkDone(ctx context.Context, id uuid.UUID, documentKey string) error
	MarkFailed(ctx context.Context, id uuid.UUID, reason string) error
	MarkRetry(ctx context.Context, id uuid.UUID, reason string) error
}

type Renderer interface {
	Render(ctx context.Context, template string, payload json.RawMessage, w io.Writer) error
}

type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) error
}

type Retrier interface {
	PublishRetry(ctx context.Context, body []byte, attempt int) error
}

// Observer receives processing outcomes; it is a seam for metrics.
type Observer interface {
	Processed(template, outcome string, renderTime time.Duration)
}

type Processor struct {
	repo     Repository
	renderer Renderer
	storage  Storage
	retrier  Retrier
	observer Observer
	log      *slog.Logger

	maxAttempts  int
	jobTimeout   time.Duration
	requeueDelay time.Duration
}

type Options struct {
	MaxAttempts int
	JobTimeout  time.Duration
	// RequeueDelay throttles redelivery when a dependency (database, broker) is unavailable,
	// so the worker does not spin on the same message.
	RequeueDelay time.Duration
}

func NewProcessor(repo Repository, r Renderer, s Storage, retrier Retrier, o Observer, log *slog.Logger, opts Options) *Processor {
	return &Processor{
		repo: repo, renderer: r, storage: s, retrier: retrier, observer: o, log: log,
		maxAttempts: opts.MaxAttempts, jobTimeout: opts.JobTimeout, requeueDelay: opts.RequeueDelay,
	}
}

const (
	OutcomeDone    = "done"
	OutcomeRetry   = "retry"
	OutcomeFailed  = "failed"
	OutcomeSkipped = "skipped"
)

// Handle processes one delivery and always settles it (ack, reject or requeue).
// Delivery is at-least-once, so the handler is idempotent: finished jobs are acknowledged as is.
func (p *Processor) Handle(ctx context.Context, msg queue.Message) {
	var m domain.GenerateMessage
	if err := json.Unmarshal(msg.Body(), &m); err != nil || m.JobID == uuid.Nil {
		p.log.Error("malformed message, sending to dead-letter queue", "body", string(msg.Body()))
		p.settle(msg.Reject())
		return
	}
	log := p.log.With("job_id", m.JobID)

	job, err := p.repo.StartProcessing(ctx, m.JobID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		log.Warn("job not found, dropping message")
		p.settle(msg.Reject())
		return
	case err != nil:
		// Infrastructure problem (e.g. database is down): let the broker redeliver.
		log.Error("start processing", "err", err)
		p.requeue(ctx, msg)
		return
	case job.Status.Terminal():
		p.observer.Processed(job.Template, OutcomeSkipped, 0)
		p.settle(msg.Ack())
		return
	}

	log = log.With("template", job.Template, "attempt", job.Attempts)
	started := time.Now()
	renderErr := p.render(ctx, job)
	elapsed := time.Since(started)

	if renderErr == nil {
		log.Info("document rendered", "duration_ms", elapsed.Milliseconds())
		p.observer.Processed(job.Template, OutcomeDone, elapsed)
		p.settle(msg.Ack())
		return
	}

	var verr *domain.ValidationError
	permanent := errors.As(renderErr, &verr) || errors.Is(renderErr, domain.ErrUnknownTemplate)
	if permanent || job.Attempts >= p.maxAttempts {
		log.Error("job failed", "err", renderErr, "permanent", permanent)
		if err := p.repo.MarkFailed(ctx, job.ID, renderErr.Error()); err != nil {
			log.Error("mark failed", "err", err)
			p.requeue(ctx, msg)
			return
		}
		p.observer.Processed(job.Template, OutcomeFailed, elapsed)
		p.settle(msg.Ack())
		return
	}

	log.Warn("job will be retried", "err", renderErr)
	if err := p.repo.MarkRetry(ctx, job.ID, renderErr.Error()); err != nil {
		log.Error("mark retry", "err", err)
	}
	if err := p.retrier.PublishRetry(ctx, msg.Body(), job.Attempts); err != nil {
		log.Error("schedule retry", "err", err)
		p.requeue(ctx, msg)
		return
	}
	p.observer.Processed(job.Template, OutcomeRetry, elapsed)
	p.settle(msg.Ack())
}

func (p *Processor) render(ctx context.Context, job domain.Job) error {
	ctx, cancel := context.WithTimeout(ctx, p.jobTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := p.renderer.Render(ctx, job.Template, job.Payload, &buf); err != nil {
		return fmt.Errorf("render: %w", err)
	}

	key := DocumentKey(job)
	if err := p.storage.Put(ctx, key, &buf); err != nil {
		return fmt.Errorf("store document: %w", err)
	}
	if err := p.repo.MarkDone(ctx, job.ID, key); err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	return nil
}

// DocumentKey spreads files across date-based directories to keep directory sizes sane.
func DocumentKey(job domain.Job) string {
	return job.CreatedAt.UTC().Format("2006/01/02") + "/" + job.ID.String() + ".pdf"
}

func (p *Processor) requeue(ctx context.Context, msg queue.Message) {
	if p.requeueDelay > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(p.requeueDelay):
		}
	}
	p.settle(msg.Requeue())
}

func (p *Processor) settle(err error) {
	if err != nil {
		p.log.Error("settle message", "err", err)
	}
}
