-- Derived forms of an asset, and the queue that produces them.
--
-- A rendition is an asset's bytes at another size. It is stored like anything
-- else -- its own content-addressed object -- and recorded here so a manifest
-- can list what exists without asking storage.
CREATE TABLE renditions (
    asset_key    TEXT NOT NULL,
    name         TEXT NOT NULL,
    key          TEXT NOT NULL,
    content_type TEXT NOT NULL,
    width        INTEGER NOT NULL,
    height       INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    digest       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (asset_key, name)
) STRICT;

-- One row per asset with work outstanding. A finished job is deleted rather
-- than marked done: the renditions themselves are the record that it ran, and
-- a table of completed work would grow forever to say nothing.
CREATE TABLE jobs (
    asset_key       TEXT PRIMARY KEY,
    state           TEXT NOT NULL CHECK (state IN ('pending', 'running', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;

CREATE INDEX jobs_ready ON jobs (state, next_attempt_at);
