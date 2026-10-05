package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
)

func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed
}

var (
	ErrNotFound        = errors.New("not found")
	ErrNotReady        = errors.New("document is not ready")
	ErrUnknownTemplate = errors.New("unknown template")
)

// ValidationError is returned when the request payload does not match the template schema.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Reason
}

type Job struct {
	ID             uuid.UUID
	Template       string
	Payload        json.RawMessage
	Status         Status
	Attempts       int
	Error          string
	DocumentKey    string
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	FinishedAt     *time.Time
}

// GenerateMessage is the body of a broker message that asks a worker to render a job.
type GenerateMessage struct {
	JobID uuid.UUID `json:"job_id"`
}
