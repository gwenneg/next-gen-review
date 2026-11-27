-- Event log. The api reads new rows (signalled with LISTEN/NOTIFY) and pushes them to WebSocket clients.
-- +goose Up
CREATE TABLE events (
    seq        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type       TEXT        NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE events;
