-- The "trusted" tier is now "contributor": the same vouched-for standing,
-- named for what those accounts are here to do. SQLite cannot alter a CHECK
-- constraint, so the table is rebuilt with the new vocabulary.
CREATE TABLE accounts_renamed (
    id         TEXT PRIMARY KEY,
    provider   TEXT NOT NULL,
    handle     TEXT NOT NULL,
    tier       TEXT NOT NULL CHECK (tier IN ('unknown', 'contributor', 'admin', 'blocked')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO accounts_renamed (id, provider, handle, tier, created_at, updated_at)
SELECT id, provider, handle,
       CASE tier WHEN 'trusted' THEN 'contributor' ELSE tier END,
       created_at, updated_at
FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_renamed RENAME TO accounts;

CREATE INDEX accounts_handle ON accounts (provider, handle);
