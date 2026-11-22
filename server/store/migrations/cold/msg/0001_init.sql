-- msg domain (cold, PostgreSQL). 0001: partitioned archive + full-text mirror.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE SCHEMA IF NOT EXISTS msg;

-- Range-partitioned archive. Content BLOB is not GIN/trgm indexable; full-text
-- goes through msg.messages_text below.
CREATE TABLE IF NOT EXISTS msg.messages_archive (
    msg_id     BIGINT      NOT NULL,
    cid        INTEGER     NOT NULL,
    seq        BIGINT      NOT NULL,
    sender_id  BIGINT      NOT NULL,
    content    BYTEA       NOT NULL,   -- Envelope payload, stored verbatim
    sent_at    BIGINT      NOT NULL,
    PRIMARY KEY (sent_at, msg_id),
    UNIQUE (cid, seq, sent_at)
) PARTITION BY RANGE (sent_at);

-- Searchable text projection (best-effort extract of plaintext parts).
CREATE TABLE IF NOT EXISTS msg.messages_text (
    msg_id     BIGINT  NOT NULL,
    cid        BIGINT  NOT NULL,
    sender_id  BIGINT  NOT NULL,
    text       TEXT,
    sent_at    BIGINT  NOT NULL,
    PRIMARY KEY (sent_at, msg_id)
) PARTITION BY RANGE (sent_at);

CREATE INDEX IF NOT EXISTS idx_messages_text_trgm ON msg.messages_text USING gin (text gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_messages_text_cid   ON msg.messages_text (cid, sent_at);

-- Monthly partition creation; works even on PG13 (no CREATE PROCEDURE needed).
CREATE OR REPLACE FUNCTION msg.ensure_month_partition(p_month TIMESTAMPTZ)
RETURNS TEXT AS $$
DECLARE
    s TIMESTAMP := date_trunc('month', p_month);
    e TIMESTAMP := s + INTERVAL '1 month';
    part  TEXT := to_char(s, '"p"YYYYMM');
    tpart TEXT := to_char(s, '"pt"YYYYMM');
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS msg.%I PARTITION OF msg.messages_archive FOR VALUES FROM (%L) TO (%L)',
        part, extract(EPOCH FROM s)::BIGINT, extract(EPOCH FROM e)::BIGINT);
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS msg.%I PARTITION OF msg.messages_text FOR VALUES FROM (%L) TO (%L)',
        tpart, extract(EPOCH FROM s)::BIGINT, extract(EPOCH FROM e)::BIGINT);
    RETURN part;
END;
$$ LANGUAGE plpgsql;

-- Initial partitions: current month, previous month, next month.
SELECT msg.ensure_month_partition(date_trunc('month', now() - INTERVAL '1 month'));
SELECT msg.ensure_month_partition(date_trunc('month', now()));
SELECT msg.ensure_month_partition(date_trunc('month', now() + INTERVAL '1 month'));
