-- +goose Up
-- What the maintainer can post as a GitHub review: the main comment and inline comments, drafted by
-- the AI and edited on the page. diff_lines are the lines GitHub accepts inline comments on, from the
-- reviewed commit's diff: {path: {"RIGHT"|"LEFT": [[first, last], ...]}}.
ALTER TABLE reviews
    ADD COLUMN diff_lines   JSONB       NOT NULL DEFAULT '{}',
    ADD COLUMN main_comment TEXT        NOT NULL DEFAULT '',
    ADD COLUMN posted_url   TEXT        NOT NULL DEFAULT '', -- the GitHub review, once posted
    ADD COLUMN posted_event TEXT        NOT NULL DEFAULT '',
    ADD COLUMN posted_at    TIMESTAMPTZ;

CREATE TABLE review_comments (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    review_id  BIGINT      NOT NULL REFERENCES reviews (id) ON DELETE CASCADE,
    path       TEXT        NOT NULL,
    line       INT         NOT NULL,
    start_line INT, -- the first line of a range
    side       TEXT        NOT NULL, -- RIGHT or LEFT
    body       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX review_comments_review ON review_comments (review_id, id);

-- +goose Down
DROP TABLE review_comments;
ALTER TABLE reviews DROP COLUMN diff_lines, DROP COLUMN main_comment, DROP COLUMN posted_url,
    DROP COLUMN posted_event, DROP COLUMN posted_at;
