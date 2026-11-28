-- +goose Up
-- One row per review the user started. Status: queued, preparing, running, then done, failed,
-- canceled or capped (stopped at the token cap).
CREATE TABLE reviews (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pr_id            TEXT        NOT NULL, -- GitHub node ID
    repo             TEXT        NOT NULL,
    number           INT         NOT NULL,
    head_sha         TEXT        NOT NULL,
    model            TEXT        NOT NULL,
    effort           TEXT        NOT NULL,
    token_cap        INT         NOT NULL,
    context          TEXT        NOT NULL, -- the user's own text, trusted
    status           TEXT        NOT NULL DEFAULT 'queued',
    cancel_requested BOOLEAN     NOT NULL DEFAULT false,
    session_id       UUID        NOT NULL DEFAULT gen_random_uuid(), -- the claude session, to join it later
    prompt           TEXT        NOT NULL DEFAULT '',
    tokens_used      INT         NOT NULL DEFAULT 0, -- cache reads not counted
    result           TEXT        NOT NULL DEFAULT '',
    error            TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);

-- At most one active review per PR.
CREATE UNIQUE INDEX reviews_one_active_per_pr ON reviews (pr_id) WHERE status IN ('queued', 'preparing', 'running');

-- +goose Down
DROP TABLE reviews;
