-- Published outbox rows are only needed for debugging; this index makes periodic cleanup cheap.
CREATE INDEX outbox_published_at_idx ON outbox (published_at) WHERE published_at IS NOT NULL;
