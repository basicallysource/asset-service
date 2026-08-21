-- The pixel dimensions of the bytes as uploaded.
--
-- These are a property of the content, not a change to it: an asset is still
-- written once and its bytes never move. They are stored so that a manifest
-- can tell a page how tall an image will be before it has loaded, which is the
-- difference between a page that settles and a page that jumps. Deriving that
-- from a rendition is not the same answer -- the ladder tops out below what a
-- camera produces, and an asset too small to shrink has no ladder at all.
--
-- Zero means not known: an asset stored before this column existed, or one
-- whose kind has no dimensions at all.
ALTER TABLE assets ADD COLUMN width INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assets ADD COLUMN height INTEGER NOT NULL DEFAULT 0;
