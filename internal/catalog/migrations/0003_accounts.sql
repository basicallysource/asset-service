-- An account is whoever is behind a token: a person, or an agent acting for
-- one, identified by something outside this service that is expensive to
-- create in bulk.
--
-- Tokens belong to accounts and limits apply to accounts, not to tokens. That
-- is the whole point: minting another token must not buy more capacity, or
-- every limit is advisory.
CREATE TABLE accounts (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    handle     TEXT NOT NULL,
    tier       TEXT NOT NULL CHECK (tier IN ('unknown', 'trusted', 'admin', 'blocked')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX accounts_handle ON accounts (provider, handle);

-- An empty account_id means an operator minted this by hand on the host, which
-- is the one kind of credential no limit applies to.
ALTER TABLE api_keys ADD COLUMN account_id TEXT NOT NULL DEFAULT '';

-- Self-served credentials expire. A token nobody remembers asking for should
-- stop working on its own.
ALTER TABLE api_keys ADD COLUMN expires_at TEXT;

-- Recorded on every asset so that usage can be counted, and so that everything
-- one account uploaded can be found in one query when it has to be.
ALTER TABLE assets ADD COLUMN account_id TEXT NOT NULL DEFAULT '';

CREATE INDEX assets_account_created_at ON assets (account_id, created_at);
