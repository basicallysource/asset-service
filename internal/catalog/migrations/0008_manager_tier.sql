-- A "manager" tier, between contributor and admin. SQLite cannot alter a CHECK
-- constraint, so the table is rebuilt with the new vocabulary, as in 0006.
CREATE TABLE accounts_with_managers (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    handle     TEXT NOT NULL,
    tier       TEXT NOT NULL CHECK (tier IN ('unknown', 'contributor', 'manager', 'admin', 'blocked')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO accounts_with_managers (id, provider, handle, tier, created_at, updated_at)
SELECT id, provider, handle, tier, created_at, updated_at FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_with_managers RENAME TO accounts;

CREATE INDEX accounts_handle ON accounts (provider, handle);
