-- +goose Up
-- Repos whose open PRs go in the queue. The name is stored as GitHub spells it.
CREATE TABLE watched_repos (
    full_name TEXT        PRIMARY KEY,
    added_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX watched_repos_full_name_lower ON watched_repos (lower(full_name));

-- The PRs waiting for the user, as of the last GitHub poll. Each poll replaces the whole table.
CREATE TABLE queue_items (
    id            TEXT        PRIMARY KEY, -- GitHub node ID
    repo          TEXT        NOT NULL,
    number        INT         NOT NULL,
    title         TEXT        NOT NULL,
    url           TEXT        NOT NULL,
    author        TEXT        NOT NULL,
    is_draft      BOOLEAN     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    additions     INT         NOT NULL,
    deletions     INT         NOT NULL,
    changed_files INT         NOT NULL,
    head_sha      TEXT        NOT NULL,
    ci_state      TEXT        NOT NULL, -- empty when the PR has no checks
    reasons       JSONB       NOT NULL  -- why it's waiting, see github.Reason
);

-- +goose Down
DROP TABLE queue_items;
DROP TABLE watched_repos;
