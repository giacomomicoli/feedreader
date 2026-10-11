-- Transcripts of YouTube videos. Fetching them takes three switches, all on:
-- FR_TRANSCRIPTS (the server's kill switch, read from the environment), the
-- global setting in app_settings (key 'transcripts', '1' = on; absent = off,
-- so neither a new install nor an upgrade makes any transcript request until
-- the user turns it on), and the source's feeds.transcripts_enabled.
--
-- A poll that stores a new entry of a YouTube feed while all three are on
-- queues it here as 'pending'; so does the entry's "Fetch transcript"
-- button. A single background worker takes the pending rows of enabled
-- sources one at a time, once they are due, and records what came of each:
-- 'ok' (the transcript), 'no_captions', or 'failed'. A row that stays
-- pending after a check (a failed attempt, a refused request, a video that
-- has not aired yet, or no captions on the first check) gets a later due
-- time. The initial add of a feed never queues anything, and turning a
-- switch on never queues the entries stored before.
--
-- entry_id is the entry's id, which is never reused (0003), and the row goes
-- with its entry: deleting the entry or its feed cascades here, so a worker
-- that fetched the transcript of an entry deleted meanwhile finds no row to
-- write into.
--
-- text is plain text, never HTML; lang is the caption track's BCP 47 code
-- (NULL when unknown); attempts counts the failed attempts so far; rechecked
-- is 1 once a check found no captions and queued the video for one more;
-- reason is why the last check left the row as it is (NULL for a row never
-- checked, and for 'ok'). Times are UTC epoch seconds: queued_at is when the
-- row was queued (or a switch turned back on), due_at when it may be fetched
-- (not before), updated_at when it was queued or last tried.

CREATE TABLE entry_transcripts (
  entry_id      INTEGER PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
  status        TEXT NOT NULL CHECK (status IN ('pending','ok','no_captions','failed')),
  lang          TEXT,
  is_generated  INTEGER NOT NULL DEFAULT 0 CHECK (is_generated IN (0, 1)),
  text          TEXT,
  attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  rechecked     INTEGER NOT NULL DEFAULT 0 CHECK (rechecked IN (0, 1)),
  reason        TEXT CHECK (reason IN ('no_captions','not_ready','refused','error','unavailable',
                                       'too_large','not_a_video','queued_too_long','attempts_exhausted')),
  queued_at     INTEGER NOT NULL,
  due_at        INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
-- The worker's next job: the pending row due first, then the one with fewer
-- attempts, then the lowest entry id (the implicit last column of the index,
-- see 0001_init.sql), read off the index in order. Rows that are done are
-- not in it.
CREATE INDEX entry_transcripts_pending ON entry_transcripts(due_at, attempts) WHERE status = 'pending';

-- The pause after YouTube refused a request: no pending row is due before
-- paused_until (UTC epoch seconds), so the worker makes no request until
-- then, also after a restart. One row at most, written by each refusal.
CREATE TABLE transcript_pause (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  paused_until  INTEGER NOT NULL,
  refusals      INTEGER NOT NULL DEFAULT 0 CHECK (refusals >= 0)
);

-- Per source: 1 = its new videos get their transcript (when the global
-- setting and FR_TRANSCRIPTS allow it). Only YouTube feeds use it.
ALTER TABLE feeds ADD COLUMN transcripts_enabled INTEGER NOT NULL DEFAULT 1 CHECK (transcripts_enabled IN (0, 1));

-- Application settings changed from the UI, one row per key. A missing key
-- has its default.
CREATE TABLE app_settings (
  key    TEXT PRIMARY KEY,
  value  TEXT NOT NULL
);
