-- Assets are immutable: a key contains a hash of the bytes it names, so a row
-- is written once and never updated. There is no updated_at for that reason.
CREATE TABLE assets (
    key          TEXT PRIMARY KEY,
    namespace    TEXT NOT NULL,
    digest       TEXT NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    filename     TEXT NOT NULL,
    visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL
) STRICT;

CREATE INDEX assets_namespace_created_at ON assets (namespace, created_at);
CREATE INDEX assets_digest ON assets (digest);

-- A token is `<prefix>_<id>_<secret>`. Only the id is stored in the clear; the
-- secret is kept as a SHA-256 so a copy of this database does not hand anyone
-- a working credential. A plain hash is right here where a password hash would
-- not be: the secret is 32 random bytes, so there is no guessing it.
CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    scopes      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT
) STRICT;
