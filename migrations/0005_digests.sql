-- Digests: custom views over several sources. A digest lists the entries of
-- the feeds it names, of the feeds in the folders it names, and the entries
-- carrying any of the tags it names, each entry once. It may also have a
-- fixed daily ingestion time at which its feeds (named or in its folders)
-- are fetched on top of their regular schedule.
--
-- Membership rows cascade from both sides: deleting a digest, a feed, a
-- folder or a tag only removes memberships. Entries are never deleted
-- because of a digest.
--
-- Names follow the folder and tag rules of 0002_unicode_name_keys.sql: name
-- keeps the spelling shown to the user, name_key (store.foldName) carries the
-- case-insensitive uniqueness and the ordering.
--
-- Digest ids are AUTOINCREMENT, never reused: a page or form left open on a
-- deleted digest must not act on a digest created later.

CREATE TABLE digests (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,  -- never reused
  name          TEXT NOT NULL,           -- as entered by the user
  name_key      TEXT NOT NULL UNIQUE,    -- case-folded name (store.foldName)
  -- Minute of the day (0 = 00:00, 1439 = 23:59) in the server's local time
  -- zone at which the digest's feeds are fetched every day; NULL = none.
  ingest_minute INTEGER CHECK (ingest_minute BETWEEN 0 AND 1439),
  position      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);

CREATE TABLE digest_feeds (
  digest_id INTEGER NOT NULL REFERENCES digests(id) ON DELETE CASCADE,
  feed_id   INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
  PRIMARY KEY (digest_id, feed_id)
);
-- The ingestion times of a feed and the ON DELETE CASCADE from feeds.
CREATE INDEX digest_feeds_feed ON digest_feeds(feed_id);

CREATE TABLE digest_folders (
  digest_id INTEGER NOT NULL REFERENCES digests(id) ON DELETE CASCADE,
  folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  PRIMARY KEY (digest_id, folder_id)
);
-- The ingestion times of a feed's folder and the ON DELETE CASCADE from
-- folders.
CREATE INDEX digest_folders_folder ON digest_folders(folder_id);

CREATE TABLE digest_tags (
  digest_id INTEGER NOT NULL REFERENCES digests(id) ON DELETE CASCADE,
  tag_id    INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (digest_id, tag_id)
);
-- The ON DELETE CASCADE from tags.
CREATE INDEX digest_tags_tag ON digest_tags(tag_id);
