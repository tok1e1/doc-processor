// Package httpapi exposes the REST API.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/domain"
	"github.com/tok1e1/doc-processor/internal/service"
)

const maxBodyBytes = 1 << 20

type JobService interface {
	Create(ctx context.Context, in service.CreateInput) (domain.Job, bool, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Job, error)
	Document(ctx context.Context, id uuid.UUID) (domain.Job, io.ReadSeekCloser, error)
}

// Checker is a dependency probed by /readyz.
type Checker interface {
	Ping(ctx context.Context) error
}

type Handler struct {
	jobs      JobService
	templates []string
	checks    map[string]Checker
	log       *slog.Logger
}

func NewHandler(jobs JobService, templates []string, checks map[string]Checker, log *slog.Logger) *Handler {
	return &Handler{jobs: jobs, templates: templates, checks: checks, log: log}
}

type createJobRequest struct {
	Template string          `json:"template"`
	Data     json.RawMessage `json:"data"`
}

type jobResponse struct {
	ID          uuid.UUID     `json:"id"`
	Template    string        `json:"template"`
	Status      domain.Status `json:"status"`
	Attempts    int           `json:"attempts"`
	Error       string        `json:"error,omitempty"`
	DocumentURL string        `json:"document_url,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
}

func toResponse(j domain.Job) jobResponse {
	resp := jobResponse{
		ID:         j.ID,
		Template:   j.Template,
		Status:     j.Status,
		Attempts:   j.Attempts,
		Error:      j.Error,
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  j.UpdatedAt,
		FinishedAt: j.FinishedAt,
	}
	if j.Status == domain.StatusDone {
		resp.DocumentURL = "/api/v1/jobs/" + j.ID.String() + "/document"
	}
	return resp
}

func (h *Handler) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req createJobRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if !errors.As(err, &maxErr) {
			err = &domain.ValidationError{Field: "body", Reason: err.Error()}
		}
		h.writeError(w, r, err)
		return
	}
	if len(req.Data) == 0 {
		h.writeError(w, r, &domain.ValidationError{Field: "data", Reason: "is required"})
		return
	}

	job, created, err := h.jobs.Create(r.Context(), service.CreateInput{
		Template:       req.Template,
		Data:           req.Data,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/v1/jobs/"+job.ID.String())
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, toResponse(job))
}

func (h *Handler) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, domain.ErrNotFound)
		return
	}
	job, err := h.jobs.Get(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(job))
}

func (h *Handler) getDocument(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, domain.ErrNotFound)
		return
	}
	job, doc, err := h.jobs.Document(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	defer doc.Close()

	modified := job.UpdatedAt
	if job.FinishedAt != nil {
		modified = *job.FinishedAt
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+job.Template+"-"+job.ID.String()+`.pdf"`)
	w.Header().Set("ETag", `"`+job.ID.String()+`"`)
	http.ServeContent(w, r, "", modified, doc)
}

func (h *Handler) listTemplates(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"templates": h.templates})
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	result := make(map[string]string, len(h.checks))
	status := http.StatusOK
	for name, c := range h.checks {
		if err := c.Ping(ctx); err != nil {
			result[name] = err.Error()
			status = http.StatusServiceUnavailable
			continue
		}
		result[name] = "ok"
	}
	writeJSON(w, status, result)
}

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		status int
		body   apiError
		verr   *domain.ValidationError
		maxErr *http.MaxBytesError
	)
	switch {
	case errors.As(err, &verr):
		status, body = http.StatusUnprocessableEntity, apiError{Code: "validation_error", Message: verr.Reason, Field: verr.Field}
	case errors.As(err, &maxErr):
		status, body = http.StatusRequestEntityTooLarge, apiError{Code: "payload_too_large", Message: "request body is too large"}
	case errors.Is(err, domain.ErrUnknownTemplate):
		status, body = http.StatusUnprocessableEntity, apiError{Code: "unknown_template", Message: err.Error(), Field: "template"}
	case errors.Is(err, domain.ErrNotFound):
		status, body = http.StatusNotFound, apiError{Code: "not_found", Message: "job not found"}
	case errors.Is(err, domain.ErrNotReady):
		status, body = http.StatusConflict, apiError{Code: "not_ready", Message: err.Error()}
	case errors.Is(err, service.ErrIdempotencyConflict):
		status, body = http.StatusConflict, apiError{Code: "idempotency_conflict", Message: err.Error()}
	default:
		h.log.ErrorContext(r.Context(), "request failed", "err", err, "path", r.URL.Path)
		status, body = http.StatusInternalServerError, apiError{Code: "internal", Message: "internal error"}
	}
	writeJSON(w, status, errorBody{Error: body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
