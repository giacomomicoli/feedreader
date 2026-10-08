-- Folder and tag names are unique and matched case-insensitively for all of
-- Unicode, not just ASCII: SQLite's NOCASE collation and LIKE fold only A-Z,
-- so 'Città' and 'CITTÀ' used to be two tags.
--
-- name keeps the spelling shown to the user; name_key is its case-folded form
-- and carries the uniqueness, lookups and ordering. The store computes keys in
-- Go (foldName in internal/store/fold.go); fold_name() here is the same Go
-- function, registered by the store as an SQL function for this backfill.
--
-- Names that already differ only in non-ASCII case are merged into the oldest
-- one (lowest id), whose spelling is kept; feeds and entry tags pointing at a
-- duplicate are moved to it. The tables are rebuilt because SQLite cannot drop
-- the old UNIQUE COLLATE NOCASE constraint in place. The store runs migrations
-- with foreign keys off, so dropping the old tables does not fire their ON
-- DELETE actions, and checks every foreign key before committing.

-- Folders ---------------------------------------------------------------

ALTER TABLE folders ADD COLUMN name_key TEXT;
UPDATE folders SET name_key = fold_name(name);

UPDATE feeds SET folder_id = (
    SELECT min(k.id) FROM folders d JOIN folders k ON k.name_key = d.name_key
    WHERE d.id = feeds.folder_id)
  WHERE folder_id IS NOT NULL;

CREATE TABLE folders_new (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,           -- as entered by the user
  name_key  TEXT NOT NULL UNIQUE,    -- case-folded name (store.foldName)
  position  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO folders_new (id, name, name_key, position)
  SELECT id, name, name_key, position FROM folders
  WHERE id IN (SELECT min(id) FROM folders GROUP BY name_key);
DROP TABLE folders;
ALTER TABLE folders_new RENAME TO folders;

-- Tags ------------------------------------------------------------------

ALTER TABLE tags ADD COLUMN name_key TEXT;
UPDATE tags SET name_key = fold_name(name);

-- An entry that carried several spellings keeps a single row.
INSERT OR IGNORE INTO entry_tags (entry_id, tag_id)
  SELECT et.entry_id, (SELECT min(k.id) FROM tags k WHERE k.name_key = d.name_key)
  FROM entry_tags et JOIN tags d ON d.id = et.tag_id;
DELETE FROM entry_tags
  WHERE tag_id NOT IN (SELECT min(id) FROM tags GROUP BY name_key);

CREATE TABLE tags_new (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,           -- as entered by the user
  name_key  TEXT NOT NULL UNIQUE     -- case-folded name (store.foldName)
);
INSERT INTO tags_new (id, name, name_key)
  SELECT id, name, name_key FROM tags
  WHERE id IN (SELECT min(id) FROM tags GROUP BY name_key);
DROP TABLE tags;
ALTER TABLE tags_new RENAME TO tags;
