-- Which work to take first. A job's cost ranks how long its derived forms take
-- to build, cheapest first, so a hundred minute-long slices of 3D models do not
-- hold up a one-second thumbnail that arrived after them. Within one cost the
-- queue is still first come, first served.
--
-- The ranks are derive.Cost's. Jobs already waiting are ranked here by content
-- type, once, so the backlog is reordered without anyone requeueing it.
ALTER TABLE jobs ADD COLUMN cost INTEGER NOT NULL DEFAULT 0;

UPDATE jobs SET cost = CASE
    WHEN (SELECT content_type FROM assets WHERE assets.key = jobs.asset_key) LIKE 'image/%' THEN 0
    WHEN (SELECT content_type FROM assets WHERE assets.key = jobs.asset_key) LIKE 'video/%' THEN 1
    WHEN (SELECT content_type FROM assets WHERE assets.key = jobs.asset_key) LIKE 'model/%' THEN 2
    ELSE 3
END;

DROP INDEX jobs_ready;
CREATE INDEX jobs_ready ON jobs (state, cost, next_attempt_at);
