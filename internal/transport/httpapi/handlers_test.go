package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
	"github.com/tok1e1/doc-processor/internal/metrics"
	"github.com/tok1e1/doc-processor/internal/service"
)

type fakeJobs struct {
	createJob     domain.Job
	createNew     bool
	createErr     error
	lastInput     service.CreateInput
	getJob        domain.Job
	getErr        error
	document      string
	documentErr   error
	documentClose bool
}

func (f *fakeJobs) Create(_ context.Context, in service.CreateInput) (domain.Job, bool, error) {
	f.lastInput = in
	return f.createJob, f.createNew, f.createErr
}

func (f *fakeJobs) Get(context.Context, uuid.UUID) (domain.Job, error) {
	return f.getJob, f.getErr
}

type closeTracker struct {
	io.ReadSeeker
	f *fakeJobs
}

func (c closeTracker) Close() error {
	c.f.documentClose = true
	return nil
}

func (f *fakeJobs) Document(context.Context, uuid.UUID) (domain.Job, io.ReadSeekCloser, error) {
	if f.documentErr != nil {
		return f.getJob, nil, f.documentErr
	}
	return f.getJob, closeTracker{strings.NewReader(f.document), f}, nil
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newServer(t *testing.T, jobs *fakeJobs, checks map[string]Checker) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(jobs, []string{"invoice", "offer_letter"}, checks, log)
	srv := httptest.NewServer(NewRouter(h, metrics.New(), log))
	t.Cleanup(srv.Close)
	return srv
}

// response is a copy of the parts of *http.Response the tests need; the body is already read and closed.
type response struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, method, url, body string, headers map[string]string) (response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error body: %s", body)
	}
	return e.Error.Code
}

func TestCreateJob(t *testing.T) {
	job := domain.Job{ID: uuid.New(), Template: "invoice", Status: domain.StatusPending, CreatedAt: time.Now()}

	t.Run("new job is accepted", func(t *testing.T) {
		jobs := &fakeJobs{createJob: job, createNew: true}
		srv := newServer(t, jobs, nil)

		resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/jobs",
			`{"template":"invoice","data":{"number":"1"}}`, map[string]string{"Idempotency-Key": "abc"})

		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if loc := resp.Header.Get("Location"); loc != "/api/v1/jobs/"+job.ID.String() {
			t.Fatalf("Location = %q", loc)
		}
		if resp.Header.Get("X-Request-ID") == "" {
			t.Fatal("X-Request-ID header is missing")
		}
		if jobs.lastInput.IdempotencyKey != "abc" || string(jobs.lastInput.Data) != `{"number":"1"}` {
			t.Fatalf("unexpected service input: %+v", jobs.lastInput)
		}
		var got jobResponse
		if err := json.Unmarshal(body, &got); err != nil || got.ID != job.ID || got.Status != domain.StatusPending {
			t.Fatalf("unexpected body %s (%v)", body, err)
		}
	})

	t.Run("repeated idempotency key returns 200", func(t *testing.T) {
		srv := newServer(t, &fakeJobs{createJob: job, createNew: false}, nil)
		resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/jobs", `{"template":"invoice","data":{}}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestCreateJobErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		svcErr   error
		wantCode int
		wantErr  string
	}{
		{"malformed json", `{"template":`, nil, http.StatusUnprocessableEntity, "validation_error"},
		{"unknown field", `{"template":"invoice","data":{},"x":1}`, nil, http.StatusUnprocessableEntity, "validation_error"},
		{"missing data", `{"template":"invoice"}`, nil, http.StatusUnprocessableEntity, "validation_error"},
		{"too large", `{"template":"invoice","data":"` + strings.Repeat("a", maxBodyBytes) + `"}`, nil, http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"unknown template", `{"template":"x","data":{}}`, domain.ErrUnknownTemplate, http.StatusUnprocessableEntity, "unknown_template"},
		{"idempotency conflict", `{"template":"invoice","data":{}}`, service.ErrIdempotencyConflict, http.StatusConflict, "idempotency_conflict"},
		{"internal error", `{"template":"invoice","data":{}}`, errors.New("db down"), http.StatusInternalServerError, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, &fakeJobs{createErr: tt.svcErr}, nil)
			resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/jobs", tt.body, nil)
			if resp.StatusCode != tt.wantCode {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tt.wantCode, body)
			}
			if code := errorCode(t, body); code != tt.wantErr {
				t.Fatalf("error code = %q, want %q", code, tt.wantErr)
			}
		})
	}
}

func TestGetJob(t *testing.T) {
	finished := time.Now()
	job := domain.Job{ID: uuid.New(), Template: "invoice", Status: domain.StatusDone, FinishedAt: &finished}
	srv := newServer(t, &fakeJobs{getJob: job}, nil)

	resp, body := do(t, http.MethodGet, srv.URL+"/api/v1/jobs/"+job.ID.String(), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got jobResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.DocumentURL != "/api/v1/jobs/"+job.ID.String()+"/document" {
		t.Fatalf("document_url = %q", got.DocumentURL)
	}

	resp, _ = do(t, http.MethodGet, srv.URL+"/api/v1/jobs/not-a-uuid", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid id: status = %d", resp.StatusCode)
	}

	srv = newServer(t, &fakeJobs{getErr: domain.ErrNotFound}, nil)
	resp, _ = do(t, http.MethodGet, srv.URL+"/api/v1/jobs/"+uuid.NewString(), "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing job: status = %d", resp.StatusCode)
	}
}

func TestGetDocument(t *testing.T) {
	job := domain.Job{ID: uuid.New(), Template: "invoice", Status: domain.StatusDone, UpdatedAt: time.Now()}
	jobs := &fakeJobs{getJob: job, document: "%PDF-1.3 content"}
	srv := newServer(t, jobs, nil)

	resp, body := do(t, http.MethodGet, srv.URL+"/api/v1/jobs/"+job.ID.String()+"/document", "", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "%PDF-1.3 content" {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !jobs.documentClose {
		t.Fatal("document reader was not closed")
	}

	resp, body = do(t, http.MethodGet, srv.URL+"/api/v1/jobs/"+job.ID.String()+"/document", "", map[string]string{"Range": "bytes=0-3"})
	if resp.StatusCode != http.StatusPartialContent || string(body) != "%PDF" {
		t.Fatalf("range request: status = %d, body = %q", resp.StatusCode, body)
	}

	srv = newServer(t, &fakeJobs{documentErr: domain.ErrNotReady}, nil)
	resp, body = do(t, http.MethodGet, srv.URL+"/api/v1/jobs/"+job.ID.String()+"/document", "", nil)
	if resp.StatusCode != http.StatusConflict || errorCode(t, body) != "not_ready" {
		t.Fatalf("not ready: status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestReadiness(t *testing.T) {
	srv := newServer(t, &fakeJobs{}, map[string]Checker{"postgres": pinger{}, "rabbitmq": pinger{}})
	if resp, _ := do(t, http.MethodGet, srv.URL+"/readyz", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	srv = newServer(t, &fakeJobs{}, map[string]Checker{"postgres": pinger{}, "rabbitmq": pinger{err: errors.New("down")}})
	resp, body := do(t, http.MethodGet, srv.URL+"/readyz", "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "down") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), recoverer(log))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}
