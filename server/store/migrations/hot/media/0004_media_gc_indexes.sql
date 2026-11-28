-- media domain (hot, SQLite). 0004: indexes for media_file GC reverse lookups.
--
-- The disk-side GC (job.MediaGC) walks media_dir and, for every file, asks
-- media_media whether any row still references it (sha256 for objects,
-- thumb_path for thumbnails). Without these indexes each lookup is a full
-- table scan — with the bounded 10000-file pass that is 10000 scans. The two
-- indexes make each reverse lookup a B-tree seek.
CREATE INDEX IF NOT EXISTS idx_media_media_sha ON media_media (sha256);
CREATE INDEX IF NOT EXISTS idx_media_media_thumb_path ON media_media (thumb_path);
