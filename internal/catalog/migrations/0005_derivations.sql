-- One row per finished derivation attempt, appended when the job leaves the
-- queue. The jobs table forgets its rows on completion -- that is what makes
-- it a queue -- so this is where "how often do we derive, how long does it
-- take, does it fail" lives. Append-only and tiny: a row is a key, a type,
-- an outcome and three timestamps, at one per attempt.
CREATE TABLE derivations (
    asset_key    TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    outcome      TEXT    NOT NULL,             -- 'ok' or 'failed'
    error        TEXT    NOT NULL DEFAULT '',
    renditions   INTEGER NOT NULL DEFAULT 0,
    attempts     INTEGER NOT NULL DEFAULT 0,
    claimed_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL,
    seconds      REAL    NOT NULL
);
CREATE INDEX derivations_finished ON derivations (finished_at);
