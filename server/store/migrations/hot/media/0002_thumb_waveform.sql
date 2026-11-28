-- media domain (hot, SQLite). 0002: inline thumbnail + waveform metadata.
--
-- thumb_data: base64 (optionally a data: URI) of a tiny preview image.
-- waveform:   JSON array of 0-100 integers (≤32 elements) for audio scrubbers.
-- Both default to '' so existing rows and the read path stay valid.
ALTER TABLE media_media ADD COLUMN thumb_data TEXT NOT NULL DEFAULT '';
ALTER TABLE media_media ADD COLUMN waveform   TEXT NOT NULL DEFAULT '';
