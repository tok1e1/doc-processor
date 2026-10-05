CREATE TABLE jobs (
    id              uuid PRIMARY KEY,
    template        text        NOT NULL,
    payload         jsonb       NOT NULL,
    status          text        NOT NULL CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0,
    error           text        NOT NULL DEFAULT '',
    document_key    text        NOT NULL DEFAULT '',
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);

CREATE UNIQUE INDEX jobs_idempotency_key_uniq ON jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Only unfinished jobs are interesting for operational queries, so keep the index small.
CREATE INDEX jobs_unfinished_idx ON jobs (status, created_at) WHERE status IN ('pending', 'processing');

CREATE TABLE outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    routing_key  text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
