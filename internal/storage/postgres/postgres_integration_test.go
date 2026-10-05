//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tok1e1/doc-processor/internal/domain"
	"github.com/tok1e1/doc-processor/internal/outbox"
)

// Run with: TEST_POSTGRES_DSN=postgres://... go test -tags integration ./internal/storage/postgres/
func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// Migrations must be safe to run concurrently (API and worker start together).
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, "TRUNCATE jobs, outbox"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newJob(key string) domain.Job {
	return domain.Job{
		ID:             uuid.New(),
		Template:       "invoice",
		Payload:        json.RawMessage(`{"number": "1"}`),
		IdempotencyKey: key,
	}
}

func countOutbox(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJobLifecycle(t *testing.T) {
	pool := setup(t)
	repo := NewJobRepository(pool)
	ctx := context.Background()

	job, created, err := repo.Create(ctx, newJob(""), "generate")
	if err != nil || !created {
		t.Fatalf("create: %v (created=%v)", err, created)
	}
	if job.Status != domain.StatusPending || job.CreatedAt.IsZero() {
		t.Fatalf("unexpected job: %+v", job)
	}
	if n := countOutbox(t, pool); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}

	started, err := repo.StartProcessing(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != domain.StatusProcessing || started.Attempts != 1 {
		t.Fatalf("after start: %+v", started)
	}

	if err := repo.MarkRetry(ctx, job.ID, "temporary"); err != nil {
		t.Fatal(err)
	}
	started, err = repo.StartProcessing(ctx, job.ID)
	if err != nil || started.Attempts != 2 {
		t.Fatalf("second attempt: %+v, %v", started, err)
	}

	if err := repo.MarkDone(ctx, job.ID, "2026/10/05/x.pdf"); err != nil {
		t.Fatal(err)
	}
	done, err := repo.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.StatusDone || done.DocumentKey != "2026/10/05/x.pdf" || done.FinishedAt == nil || done.Error != "" {
		t.Fatalf("after done: %+v", done)
	}

	// A redelivered message must not restart a finished job.
	again, err := repo.StartProcessing(ctx, job.ID)
	if err != nil || again.Status != domain.StatusDone || again.Attempts != 2 {
		t.Fatalf("finished job was modified: %+v, %v", again, err)
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	pool := setup(t)
	repo := NewJobRepository(pool)
	ctx := context.Background()

	first, created, err := repo.Create(ctx, newJob("order-42"), "generate")
	if err != nil || !created {
		t.Fatalf("first create: %v", err)
	}
	second, created, err := repo.Create(ctx, newJob("order-42"), "generate")
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("expected existing job %s, got %s (created=%v)", first.ID, second.ID, created)
	}
	if n := countOutbox(t, pool); n != 1 {
		t.Fatalf("duplicate request must not produce a second message, outbox rows = %d", n)
	}
}

func TestNotFound(t *testing.T) {
	repo := NewJobRepository(setup(t))
	ctx := context.Background()

	if _, err := repo.Get(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get: want ErrNotFound, got %v", err)
	}
	if _, err := repo.StartProcessing(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("StartProcessing: want ErrNotFound, got %v", err)
	}
	if err := repo.MarkDone(ctx, uuid.New(), "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("MarkDone: want ErrNotFound, got %v", err)
	}
}

func TestOutboxConcurrentRelays(t *testing.T) {
	pool := setup(t)
	repo := NewJobRepository(pool)
	box := NewOutboxRepository(pool)
	ctx := context.Background()

	const jobs = 50
	for i := 0; i < jobs; i++ {
		if _, _, err := repo.Create(ctx, newJob(""), "generate"); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	publish := func(_ context.Context, msgs []outbox.Message) ([]int64, error) {
		ids := make([]int64, 0, len(msgs))
		mu.Lock()
		defer mu.Unlock()
		for _, m := range msgs {
			seen[m.ID]++
			ids = append(ids, m.ID)
		}
		time.Sleep(5 * time.Millisecond) // keep rows locked a little to provoke contention
		return ids, nil
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := box.ProcessBatch(ctx, 7, publish)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != jobs {
		t.Fatalf("published %d distinct messages, want %d", len(seen), jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("message %d published %d times", id, n)
		}
	}

	deleted, err := box.Cleanup(ctx, time.Now().Add(time.Minute))
	if err != nil || deleted != jobs {
		t.Fatalf("cleanup deleted %d rows (%v), want %d", deleted, err, jobs)
	}
}

func TestOutboxPartialFailureKeepsRemainingMessages(t *testing.T) {
	pool := setup(t)
	repo := NewJobRepository(pool)
	box := NewOutboxRepository(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, _, err := repo.Create(ctx, newJob(""), "generate"); err != nil {
			t.Fatal(err)
		}
	}

	brokerDown := errors.New("broker down")
	n, err := box.ProcessBatch(ctx, 10, func(_ context.Context, msgs []outbox.Message) ([]int64, error) {
		return []int64{msgs[0].ID}, brokerDown
	})
	if !errors.Is(err, brokerDown) || n != 1 {
		t.Fatalf("got n=%d err=%v", n, err)
	}

	var pending int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE published_at IS NULL").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("pending = %d, want 2", pending)
	}
}
