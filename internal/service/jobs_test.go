package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
)

type fakeRepo struct {
	jobs  map[uuid.UUID]domain.Job
	byKey map[string]uuid.UUID
	err   error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{jobs: map[uuid.UUID]domain.Job{}, byKey: map[string]uuid.UUID{}}
}

func (r *fakeRepo) Create(_ context.Context, job domain.Job, _ string) (domain.Job, bool, error) {
	if r.err != nil {
		return domain.Job{}, false, r.err
	}
	if id, ok := r.byKey[job.IdempotencyKey]; ok && job.IdempotencyKey != "" {
		return r.jobs[id], false, nil
	}
	r.jobs[job.ID] = job
	if job.IdempotencyKey != "" {
		r.byKey[job.IdempotencyKey] = job.ID
	}
	return job, true, nil
}

func (r *fakeRepo) Get(_ context.Context, id uuid.UUID) (domain.Job, error) {
	j, ok := r.jobs[id]
	if !ok {
		return domain.Job{}, domain.ErrNotFound
	}
	return j, nil
}

type fakeValidator struct{ err error }

func (v fakeValidator) Validate(string, json.RawMessage) error { return v.err }

type fakeDocs struct{ content string }

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

func (d fakeDocs) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nopCloser{strings.NewReader(d.content)}, nil
}

func TestCreate(t *testing.T) {
	repo := newFakeRepo()
	var created []string
	svc := NewJobs(repo, fakeValidator{}, fakeDocs{}, "generate")
	svc.OnCreated = func(tmpl string) { created = append(created, tmpl) }

	job, isNew, err := svc.Create(context.Background(), CreateInput{Template: "invoice", Data: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !isNew || job.Status != domain.StatusPending || job.ID == uuid.Nil {
		t.Fatalf("unexpected job: %+v (new=%v)", job, isNew)
	}
	if len(created) != 1 || created[0] != "invoice" {
		t.Fatalf("OnCreated hook not called correctly: %v", created)
	}
}

func TestCreateIdempotency(t *testing.T) {
	svc := NewJobs(newFakeRepo(), fakeValidator{}, fakeDocs{}, "generate")
	ctx := context.Background()

	first, _, err := svc.Create(ctx, CreateInput{Template: "invoice", Data: json.RawMessage(`{"a":1,"b":2}`), IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("same request returns the existing job", func(t *testing.T) {
		// Key order differs on purpose: jsonb does not preserve it.
		again, isNew, err := svc.Create(ctx, CreateInput{Template: "invoice", Data: json.RawMessage(`{"b":2, "a":1}`), IdempotencyKey: "k1"})
		if err != nil {
			t.Fatal(err)
		}
		if isNew || again.ID != first.ID {
			t.Fatalf("expected the same job %s, got %s (new=%v)", first.ID, again.ID, isNew)
		}
	})

	t.Run("different payload with the same key is a conflict", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{Template: "invoice", Data: json.RawMessage(`{"a":2}`), IdempotencyKey: "k1"})
		if !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("want ErrIdempotencyConflict, got %v", err)
		}
	})

	t.Run("key length is limited", func(t *testing.T) {
		_, _, err := svc.Create(ctx, CreateInput{Template: "invoice", Data: json.RawMessage(`{}`), IdempotencyKey: strings.Repeat("x", 200)})
		var verr *domain.ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("want ValidationError, got %v", err)
		}
	})
}

func TestCreatePropagatesErrors(t *testing.T) {
	verr := &domain.ValidationError{Field: "number", Reason: "is required"}
	svc := NewJobs(newFakeRepo(), fakeValidator{err: verr}, fakeDocs{}, "generate")
	if _, _, err := svc.Create(context.Background(), CreateInput{Template: "invoice"}); !errors.Is(err, verr) {
		t.Fatalf("want validation error, got %v", err)
	}

	repo := newFakeRepo()
	repo.err = errors.New("db is down")
	svc = NewJobs(repo, fakeValidator{}, fakeDocs{}, "generate")
	if _, _, err := svc.Create(context.Background(), CreateInput{Template: "invoice"}); !errors.Is(err, repo.err) {
		t.Fatalf("want repository error, got %v", err)
	}
}

func TestDocument(t *testing.T) {
	repo := newFakeRepo()
	pending := domain.Job{ID: uuid.New(), Status: domain.StatusProcessing}
	done := domain.Job{ID: uuid.New(), Status: domain.StatusDone, DocumentKey: "k.pdf"}
	repo.jobs[pending.ID] = pending
	repo.jobs[done.ID] = done
	svc := NewJobs(repo, fakeValidator{}, fakeDocs{content: "%PDF-1.3"}, "generate")
	ctx := context.Background()

	if _, _, err := svc.Document(ctx, pending.ID); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	if _, _, err := svc.Document(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	_, r, err := svc.Document(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "%PDF-1.3" {
		t.Fatalf("unexpected content %q", b)
	}
}
