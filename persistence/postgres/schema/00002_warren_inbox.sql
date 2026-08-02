-- +goose Up
-- Consumer-side deduplication. The row is written in the HANDLER's own
-- transaction, which is what closes the crash-after-success duplicate window
-- an in-memory store cannot.
CREATE TABLE IF NOT EXISTS warren_inbox (
    id         TEXT        PRIMARY KEY,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS warren_inbox_expires_at ON warren_inbox (expires_at);

-- +goose Down
DROP TABLE IF EXISTS warren_inbox;
