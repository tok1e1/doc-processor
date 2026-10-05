package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
)

type settlement string

const (
	acked     settlement = "ack"
	rejected  settlement = "reject"
	requeued  settlement = "requeue"
	unsettled settlement = ""
)

type fakeMessage struct {
	body []byte
	mu   sync.Mutex
	got  settlement
}

func newMessage(t *testing.T, id uuid.UUID) *fakeMessage {
	t.Helper()
	b, err := json.Marshal(domain.GenerateMessage{JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeMessage{body: b}
}

func (m *fakeMessage) Body() []byte   { return m.body }
func (m *fakeMessage) Ack() error     { return m.settle(acked) }
func (m *fakeMessage) Reject() error  { return m.settle(rejected) }
func (m *fakeMessage) Requeue() error { return m.settle(requeued) }

func (m *fakeMessage) settle(s settlement) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.got != unsettled {
		return errors.New("message settled twice")
	}
	m.got = s
	return nil
}

func (m *fakeMessage) result() settlement {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.got
}

type fakeRepo struct {
	job      domain.Job
	startErr error
	done     string
	failed   string
	retried  string
}

func (r *fakeRepo) StartProcessing(_ context.Context, id uuid.UUID) (domain.Job, error) {
	if r.startErr != nil {
		return domain.Job{}, r.startErr
	}
	if id != r.job.ID {
		return domain.Job{}, domain.ErrNotFound
	}
	if !r.job.Status.Terminal() {
		r.job.Status = domain.StatusProcessing
		r.job.Attempts++
	}
	return r.job, nil
}

func (r *fakeRepo) MarkDone(_ context.Context, _ uuid.UUID, key string) error {
	r.done = key
	return nil
}

func (r *fakeRepo) MarkFailed(_ context.Context, _ uuid.UUID, reason string) error {
	r.failed = reason
	return nil
}

func (r *fakeRepo) MarkRetry(_ context.Context, _ uuid.UUID, reason string) error {
	r.retried = reason
	return nil
}

type fakeRenderer struct{ err error }

func (f fakeRenderer) Render(_ context.Context, _ string, _ json.RawMessage, w io.Writer) error {
	if f.err != nil {
		return f.err
	}
	_, err := w.Write([]byte("%PDF-1.3 fake"))
	return err
}

type fakeStorage struct{ objects map[string][]byte }

func (s *fakeStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	s.objects[key] = b
	return err
}

type fakeRetrier struct {
	attempts []int
	err      error
}

func (f *fakeRetrier) PublishRetry(_ context.Context, _ []byte, attempt int) error {
	f.attempts = append(f.attempts, attempt)
	return f.err
}

type recorder struct{ outcomes []string }

func (r *recorder) Processed(_, outcome string, _ time.Duration) {
	r.outcomes = append(r.outcomes, outcome)
}

type env struct {
	repo     *fakeRepo
	storage  *fakeStorage
	retrier  *fakeRetrier
	observer *recorder
	proc     *Processor
}

func newEnv(job domain.Job, renderErr error) *env {
	e := &env{
		repo:     &fakeRepo{job: job},
		storage:  &fakeStorage{objects: map[string][]byte{}},
		retrier:  &fakeRetrier{},
		observer: &recorder{},
	}
	e.proc = NewProcessor(e.repo, fakeRenderer{err: renderErr}, e.storage, e.retrier, e.observer,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Options{MaxAttempts: 3, JobTimeout: time.Second})
	return e
}

func pendingJob(attempts int) domain.Job {
	return domain.Job{
		ID:        uuid.New(),
		Template:  "invoice",
		Status:    domain.StatusPending,
		Attempts:  attempts,
		CreatedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}
}

func TestHandleSuccess(t *testing.T) {
	job := pendingJob(0)
	e := newEnv(job, nil)
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if msg.result() != acked {
		t.Fatalf("want ack, got %q", msg.result())
	}
	wantKey := "2026/10/05/" + job.ID.String() + ".pdf"
	if e.repo.done != wantKey {
		t.Fatalf("MarkDone key = %q, want %q", e.repo.done, wantKey)
	}
	if string(e.storage.objects[wantKey]) != "%PDF-1.3 fake" {
		t.Fatal("document was not stored")
	}
	if e.observer.outcomes[0] != OutcomeDone {
		t.Fatalf("outcome = %v", e.observer.outcomes)
	}
}

func TestHandleTransientErrorIsRetried(t *testing.T) {
	job := pendingJob(0)
	e := newEnv(job, errors.New("disk full"))
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if msg.result() != acked {
		t.Fatalf("original message must be acked after scheduling a retry, got %q", msg.result())
	}
	if len(e.retrier.attempts) != 1 || e.retrier.attempts[0] != 1 {
		t.Fatalf("retry attempts = %v, want [1]", e.retrier.attempts)
	}
	if e.repo.retried == "" || e.repo.failed != "" {
		t.Fatalf("job must be marked for retry, not failed: %+v", e.repo)
	}
}

func TestHandleFailsAfterMaxAttempts(t *testing.T) {
	job := pendingJob(2) // the third attempt is the last one
	e := newEnv(job, errors.New("disk full"))
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if msg.result() != acked {
		t.Fatalf("want ack, got %q", msg.result())
	}
	if e.repo.failed == "" {
		t.Fatal("job must be marked as failed")
	}
	if len(e.retrier.attempts) != 0 {
		t.Fatal("no retry expected after the last attempt")
	}
}

func TestHandleValidationErrorIsPermanent(t *testing.T) {
	job := pendingJob(0)
	e := newEnv(job, &domain.ValidationError{Field: "date", Reason: "bad"})
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if e.repo.failed == "" || len(e.retrier.attempts) != 0 {
		t.Fatalf("validation errors must fail the job without retries: %+v", e.repo)
	}
}

func TestHandleSkipsFinishedJobs(t *testing.T) {
	job := pendingJob(1)
	job.Status = domain.StatusDone
	e := newEnv(job, nil)
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if msg.result() != acked || e.repo.done != "" {
		t.Fatalf("duplicate delivery must be acked without rendering, got %q", msg.result())
	}
	if e.observer.outcomes[0] != OutcomeSkipped {
		t.Fatalf("outcome = %v", e.observer.outcomes)
	}
}

func TestHandleSettlement(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte
		startErr error
		want     settlement
	}{
		{"malformed json", []byte("{not json"), nil, rejected},
		{"empty job id", []byte(`{"job_id":"00000000-0000-0000-0000-000000000000"}`), nil, rejected},
		{"unknown job", nil, domain.ErrNotFound, rejected},
		{"database unavailable", nil, errors.New("connection refused"), requeued},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := pendingJob(0)
			e := newEnv(job, nil)
			e.repo.startErr = tt.startErr
			msg := newMessage(t, job.ID)
			if tt.body != nil {
				msg.body = tt.body
			}

			e.proc.Handle(context.Background(), msg)

			if msg.result() != tt.want {
				t.Fatalf("want %q, got %q", tt.want, msg.result())
			}
		})
	}
}

func TestHandleRequeuesWhenRetryCannotBeScheduled(t *testing.T) {
	job := pendingJob(0)
	e := newEnv(job, errors.New("disk full"))
	e.retrier.err = errors.New("broker unavailable")
	msg := newMessage(t, job.ID)

	e.proc.Handle(context.Background(), msg)

	if msg.result() != requeued {
		t.Fatalf("want requeue, got %q", msg.result())
	}
}
