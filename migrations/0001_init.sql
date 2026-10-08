-- Initial schema: folders, feeds, entries, tags and entry_tags.
--
-- Timestamps are UTC Unix epoch seconds (INTEGER); NULL means "never" and maps
-- to a zero time.Time in Go. Connection pragmas (WAL, foreign_keys,
-- busy_timeout, synchronous) are set per connection by the store's DSN, not
-- here: journal_mode cannot change inside the migration transaction.
--
-- Index note: the implicit rowid (id) is the last, ascending column of every
-- index, so an ascending (…, published_at) index scanned backwards yields the
-- grid order "published_at DESC, id DESC" without a sort step. Do not declare
-- published_at DESC in these indexes.

CREATE TABLE folders (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  position  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE feeds (
  id              INTEGER PRIMARY KEY,
  kind            TEXT NOT NULL CHECK (kind IN ('rss','youtube')),
  url             TEXT NOT NULL UNIQUE,
  site_url        TEXT,
  title           TEXT NOT NULL,
  original_title  TEXT,
  icon_url        TEXT,
  folder_id       INTEGER REFERENCES folders(id) ON DELETE SET NULL,
  etag            TEXT,
  last_modified   TEXT,
  last_fetched_at INTEGER,
  next_fetch_at   INTEGER NOT NULL DEFAULT 0,  -- 0 = due immediately
  interval_sec    INTEGER,                     -- NULL = use global poll interval
  error_count     INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  created_at      INTEGER NOT NULL
);
CREATE INDEX feeds_next_fetch ON feeds(next_fetch_at);
CREATE INDEX feeds_folder     ON feeds(folder_id);

CREATE TABLE entries (
  id            INTEGER PRIMARY KEY,
  feed_id       INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
  guid          TEXT NOT NULL,              -- exact, case-sensitive (BINARY)
  url           TEXT,
  title         TEXT NOT NULL,
  summary_html  TEXT,
  author        TEXT,
  thumbnail_url TEXT,
  published_at  INTEGER NOT NULL,
  updated_at    INTEGER,
  fetched_at    INTEGER NOT NULL,           -- first time the entry was stored
  is_read       INTEGER NOT NULL DEFAULT 0,
  is_later      INTEGER NOT NULL DEFAULT 0,
  is_favourite  INTEGER NOT NULL DEFAULT 0,
  read_at       INTEGER,
  UNIQUE (feed_id, guid)                    -- dedup is per feed only
);
-- Single-feed and folder scopes; also serves the ON DELETE CASCADE from feeds.
CREATE INDEX entries_feed_pub  ON entries(feed_id, published_at);
-- "All" scope with the Unread filter.
CREATE INDEX entries_read_pub  ON entries(is_read, published_at);
-- "All" scope without the Unread filter.
CREATE INDEX entries_pub       ON entries(published_at);
-- Watch later / Read later and Favourites scopes.
CREATE INDEX entries_later     ON entries(published_at) WHERE is_later = 1;
CREATE INDEX entries_favourite ON entries(published_at) WHERE is_favourite = 1;
-- Single-feed scope with the Unread filter, and sidebar unread counts.
CREATE INDEX entries_unread    ON entries(feed_id, published_at) WHERE is_read = 0;

CREATE TABLE tags (
  id    INTEGER PRIMARY KEY,
  name  TEXT NOT NULL UNIQUE COLLATE NOCASE
);

CREATE TABLE entry_tags (
  entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
  tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (entry_id, tag_id)
);
-- Tag scope, tag counts and the ON DELETE CASCADE from tags.
CREATE INDEX entry_tags_tag ON entry_tags(tag_id);
