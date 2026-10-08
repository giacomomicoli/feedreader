-- Entry ids only ever grow. "Mark all as read" sweeps the unread entries
-- whose id is at most the largest id that existed when the page was rendered
-- (Store.MaxEntryID / MarkScopeReadUpTo): writers are serialized, so anything
-- committed later gets a larger id. A plain INTEGER PRIMARY KEY hands out
-- max(id) + 1, which reuses the ids of the newest entries once they are
-- deleted (unsubscribing a feed), so new, unseen entries could fall under an
-- old watermark. AUTOINCREMENT never reuses an id.
--
-- SQLite cannot add AUTOINCREMENT in place, so the table is rebuilt with the
-- same columns, ids and indexes (see 0001_init.sql for the index notes). The
-- store runs migrations with foreign keys off, so dropping the old table does
-- not cascade to entry_tags, and checks every foreign key before committing.

CREATE TABLE entries_new (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused
  feed_id       INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
  guid          TEXT NOT NULL,              -- exact, case-sensitive (BINARY)
  url           TEXT,
  title         TEXT NOT NULL,
  summary_html  TEXT,
  author        TEXT,
  thumbnail_url TEXT,
  published_at  INTEGER NOT NULL,
  updated_at    INTEGER,
  fetched_at    INTEGER NOT NULL,           -- fetch time of the response that first delivered it
  is_read       INTEGER NOT NULL DEFAULT 0,
  is_later      INTEGER NOT NULL DEFAULT 0,
  is_favourite  INTEGER NOT NULL DEFAULT 0,
  read_at       INTEGER,
  UNIQUE (feed_id, guid)                    -- dedup is per feed only
);
INSERT INTO entries_new (id, feed_id, guid, url, title, summary_html, author, thumbnail_url,
    published_at, updated_at, fetched_at, is_read, is_later, is_favourite, read_at)
  SELECT id, feed_id, guid, url, title, summary_html, author, thumbnail_url,
    published_at, updated_at, fetched_at, is_read, is_later, is_favourite, read_at
  FROM entries;
DROP TABLE entries;
ALTER TABLE entries_new RENAME TO entries;

CREATE INDEX entries_feed_pub  ON entries(feed_id, published_at);
CREATE INDEX entries_read_pub  ON entries(is_read, published_at);
CREATE INDEX entries_pub       ON entries(published_at);
CREATE INDEX entries_later     ON entries(published_at) WHERE is_later = 1;
CREATE INDEX entries_favourite ON entries(published_at) WHERE is_favourite = 1;
CREATE INDEX entries_unread    ON entries(feed_id, published_at) WHERE is_read = 0;
