-- +goose Up
-- The transactional outbox. Rows are written in the SAME transaction as the
-- aggregate state whose events they carry: that atomicity is the pattern.
CREATE TABLE IF NOT EXISTS warren_outbox (
    id           BIGSERIAL PRIMARY KEY,
    message_id   TEXT,
    topic        TEXT        NOT NULL,
    type         TEXT        NOT NULL,
    key          TEXT        NOT NULL DEFAULT '',
    payload      BYTEA       NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    failed_at    TIMESTAMPTZ,
    error        TEXT
);

-- The relay's only query: undispatched rows in insertion order. A PARTIAL
-- index keeps it the size of the backlog rather than the size of history, so
-- a table with ten million published rows still drains in microseconds.
CREATE INDEX IF NOT EXISTS warren_outbox_pending
    ON warren_outbox (id)
    WHERE published_at IS NULL AND failed_at IS NULL;

-- Retention sweeps by published_at.
CREATE INDEX IF NOT EXISTS warren_outbox_published_at
    ON warren_outbox (published_at)
    WHERE published_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS warren_outbox;
