# doc-processor

[![CI](https://github.com/tok1e1/doc-processor/actions/workflows/ci.yml/badge.svg)](https://github.com/tok1e1/doc-processor/actions/workflows/ci.yml)

An asynchronous PDF generation service written in Go. Clients submit a template name and
a JSON payload, get a job ID right away, and download the document once a worker has rendered it.

It's modelled on a problem I've solved in production: document generation was a bottleneck in a
monolith, so it was moved into a separate service with a queue and horizontally scalable workers.

**Stack:** Go 1.23 · PostgreSQL · RabbitMQ · Prometheus · Docker · GitHub Actions

## Architecture

```mermaid
flowchart LR
    client([Client]) -->|POST /api/v1/jobs| api[API]
    api -->|job + outbox row<br/>in one transaction| pg[(PostgreSQL)]
    relay[Outbox relay] -->|SKIP LOCKED batch| pg
    relay -->|publisher confirms| ex{{documents exchange}}
    ex --> q[documents.generate]
    q --> w1[Worker pool]
    w1 -->|render PDF| fs[(Document storage)]
    w1 -->|status| pg
    w1 -.->|transient error| rq[documents.retry.Nms<br/>TTL queues]
    rq -.->|dead-letter back| ex
    q -.->|poison message| dlq[documents.dead]
    client -->|GET /api/v1/jobs/:id/document| api
    api --> fs
```

1. The API validates the payload against the template schema synchronously, so bad requests fail
   fast with `422` and never reach the queue.
2. The job and its outbox message are written **in the same transaction**. A background relay
   publishes outbox rows to RabbitMQ with publisher confirms and marks them as sent.
3. Workers consume with `prefetch = pool size`, render the PDF, write it atomically to storage
   and mark the job `done`.
4. Transient failures are retried with exponential back-off; validation errors and exhausted
   attempts mark the job `failed`. Malformed messages go to a dead-letter queue.

## Design decisions

| Problem | Decision |
|---|---|
| A job saved without a message (or a message without a job) if the process dies between the DB write and the publish | **Transactional outbox.** The relay uses `FOR UPDATE SKIP LOCKED`, so several API replicas can relay in parallel without duplicates. |
| The broker delivers at least once, so duplicates are expected | **Idempotent consumer.** `StartProcessing` only moves jobs that are not finished; a redelivered message for a `done` job is acknowledged and skipped. |
| Client retries on timeouts create duplicate documents | **`Idempotency-Key` header** backed by a partial unique index. Same key with a different payload returns `409`. |
| Delayed retries with per-message TTL suffer from head-of-line blocking (RabbitMQ expires messages only at the head of the queue) | **One retry queue per delay** (`documents.retry.2000ms`, `…4000ms`, …) with a fixed `x-message-ttl`, dead-lettered back to the main exchange. |
| Database or broker is down, and the worker spins on the same message | Infrastructure errors requeue the message after a short delay, which also acts as back-pressure. |
| A poison message crashes the worker in a loop | The pool recovers from panics and rejects the message to `documents.dead`. |
| Readers see a half-written PDF | Storage writes to a temp file, `fsync`s it and renames it. |
| Rolling deploys lose in-flight work | **Graceful shutdown:** the consumer is cancelled first, prefetched messages are still processed and acked, then the connection is closed. Anything unfinished within `SHUTDOWN_TIMEOUT` is redelivered by RabbitMQ. |
| Float rounding in money | Amounts are integers in minor units (kopecks/cents); VAT is rounded half up with integer arithmetic, and input limits keep every sum inside `int64`. |
| Cyrillic text in PDFs | DejaVu Sans is embedded as a UTF-8 font. |

## Quick start

```bash
docker compose up -d --build --wait   # postgres, rabbitmq, api, 2 workers, prometheus
make smoke                            # end-to-end check
```

Create a job:

```bash
curl -s -X POST localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: invoice-42' \
  --data @examples/invoice.json
```

```json
{
  "id": "6f1c3a52-6d0e-4e0a-9a7b-1f0d2c9e5b11",
  "template": "invoice",
  "status": "pending",
  "attempts": 0,
  "created_at": "2026-10-05T12:00:00.123Z",
  "updated_at": "2026-10-05T12:00:00.123Z"
}
```

Check the status and download the result:

```bash
curl -s localhost:8080/api/v1/jobs/<id>
curl -s localhost:8080/api/v1/jobs/<id>/document -o invoice.pdf
```

* RabbitMQ UI: http://localhost:15672 (guest / guest)
* Prometheus: http://localhost:9091

## API

The full spec is in [`api/openapi.yaml`](api/openapi.yaml).

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/jobs` | Create a job. `202` for a new job, `200` for a repeated `Idempotency-Key`. |
| `GET` | `/api/v1/jobs/{id}` | Job status: `pending`, `processing`, `done`, `failed`. |
| `GET` | `/api/v1/jobs/{id}/document` | The PDF. Supports `Range` and conditional requests; `409` while not ready. |
| `GET` | `/api/v1/templates` | Available templates. |
| `GET` | `/healthz`, `/readyz` | Liveness, and readiness (checks Postgres and RabbitMQ). |

Errors share one format:

```json
{ "error": { "code": "validation_error", "message": "must be a date in YYYY-MM-DD format", "field": "date" } }
```

Templates: `invoice` and `offer_letter`. Example payloads are in [`examples/`](examples).
To add a template, implement `render.Template` (`Name`, `Validate`, `Render`) and register it in `render.Default()`.

## Configuration

All settings come from environment variables.

| Variable | Default | Description |
|---|---|---|
| `HTTP_ADDR` | `:8080` | API listen address |
| `METRICS_ADDR` | `:9090` | Prometheus metrics address (API and worker) |
| `POSTGRES_DSN` | `postgres://docs:docs@localhost:5432/docs?sslmode=disable` | |
| `POSTGRES_MAX_CONNS` | `10` | Connection pool size |
| `RABBITMQ_URL` | `amqp://guest:guest@localhost:5672/` | |
| `STORAGE_DIR` | `./data/documents` | Directory for rendered documents |
| `WORKER_CONCURRENCY` | `8` | Worker pool size and RabbitMQ prefetch |
| `JOB_TIMEOUT` | `30s` | Rendering timeout for a single job |
| `MAX_ATTEMPTS` | `5` | Attempts before a job is marked `failed` |
| `RETRY_BASE_DELAY` | `2s` | First retry delay; doubles each attempt, capped at 1 minute |
| `OUTBOX_INTERVAL` | `500ms` | Outbox polling interval |
| `OUTBOX_BATCH_SIZE` | `100` | Messages per relay batch |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful shutdown deadline |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Observability

* Structured JSON logs (`log/slog`) with `request_id` and `job_id`.
* Prometheus metrics with the `docproc_` prefix:
  * `http_requests_total`, `http_request_duration_seconds` — labelled by route pattern, so cardinality stays bounded
  * `jobs_created_total`, `jobs_processed_total{outcome="done|retry|failed|skipped"}`
  * `render_duration_seconds`, `workers_busy`, `outbox_published_total`

## Development

```bash
make test               # unit tests with -race
make test-integration   # repository tests against a real Postgres (docker compose up -d postgres)
make lint               # golangci-lint
make cover
```

CI runs lint, unit tests, integration tests against a Postgres service container, and an
end-to-end smoke test against the full `docker compose` stack.

## Project layout

```
cmd/
  api/            HTTP API and outbox relay
  worker/         queue consumer and worker pool
internal/
  domain/         entities and domain errors
  service/        API use cases (create, get, download)
  worker/         message processing and worker pool
  render/         templates and PDF rendering
  outbox/         outbox relay
  storage/postgres/  repositories, embedded migrations
  queue/rabbitmq/    topology, publisher with confirms, reconnecting consumer
  transport/httpapi/ handlers, middleware
  blob/           document storage
  metrics/        Prometheus collectors
api/openapi.yaml
```

## Roadmap

* S3-compatible storage (the `blob` interface is ready for it) and pre-signed download URLs.
* A reaper for jobs stuck in `processing` after a worker crash.
* Monthly partitioning of `jobs` with retention for old partitions.
* OpenTelemetry tracing across API → broker → worker.

## License

MIT. DejaVu fonts are distributed under their own [license](internal/render/fonts/LICENSE).
