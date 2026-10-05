// Package service contains the application logic for the API side.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
)

const maxIdempotencyKeyLen = 128

var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")

type Repository interface {
	Create(ctx context.Context, job domain.Job, routingKey string) (domain.Job, bool, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Job, error)
}

type Validator interface {
	Validate(template string, payload json.RawMessage) error
}

type Documents interface {
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)
}

type Jobs struct {
	repo       Repository
	validator  Validator
	documents  Documents
	routingKey string

	// OnCreated is called for every newly accepted job (metrics hook).
	OnCreated func(template string)
}

func NewJobs(repo Repository, validator Validator, documents Documents, routingKey string) *Jobs {
	return &Jobs{repo: repo, validator: validator, documents: documents, routingKey: routingKey}
}

type CreateInput struct {
	Template       string
	Data           json.RawMessage
	IdempotencyKey string
}

// Create validates the request and stores the job. created=false means an existing job
// was returned for a repeated idempotency key.
func (s *Jobs) Create(ctx context.Context, in CreateInput) (job domain.Job, created bool, err error) {
	if len(in.IdempotencyKey) > maxIdempotencyKeyLen {
		return domain.Job{}, false, &domain.ValidationError{Field: "Idempotency-Key", Reason: "is too long"}
	}
	if err := s.validator.Validate(in.Template, in.Data); err != nil {
		return domain.Job{}, false, err
	}

	job, created, err = s.repo.Create(ctx, domain.Job{
		ID:             uuid.New(),
		Template:       in.Template,
		Payload:        in.Data,
		Status:         domain.StatusPending,
		IdempotencyKey: in.IdempotencyKey,
	}, s.routingKey)
	if err != nil {
		return domain.Job{}, false, err
	}

	if !created && !sameRequest(job, in) {
		return domain.Job{}, false, ErrIdempotencyConflict
	}
	if created && s.OnCreated != nil {
		s.OnCreated(job.Template)
	}
	return job, created, nil
}

func (s *Jobs) Get(ctx context.Context, id uuid.UUID) (domain.Job, error) {
	return s.repo.Get(ctx, id)
}

// Document returns the rendered PDF. The caller must close the reader.
func (s *Jobs) Document(ctx context.Context, id uuid.UUID) (domain.Job, io.ReadSeekCloser, error) {
	job, err := s.repo.Get(ctx, id)
	if err != nil {
		return domain.Job{}, nil, err
	}
	if job.Status != domain.StatusDone {
		return job, nil, domain.ErrNotReady
	}
	r, err := s.documents.Open(ctx, job.DocumentKey)
	if err != nil {
		return job, nil, err
	}
	return job, r, nil
}

// sameRequest compares payloads semantically: Postgres jsonb does not preserve formatting or key order.
func sameRequest(job domain.Job, in CreateInput) bool {
	if job.Template != in.Template {
		return false
	}
	var a, b any
	if json.Unmarshal(job.Payload, &a) != nil || json.Unmarshal(in.Data, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
