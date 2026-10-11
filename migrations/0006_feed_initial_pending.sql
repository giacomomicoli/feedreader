-- A subscription stored before its feed could be fetched (a YouTube channel
-- whose feed was not answering when it was added) owes the initial-add rule
-- to its first successful fetch: that fetch stores its entries with only the
-- newest few unread and the rest read, as the add flow would have. The flag
-- is cleared in the same transaction. 0 for every existing feed, so they keep
-- polling exactly as before.
ALTER TABLE feeds ADD COLUMN initial_pending INTEGER NOT NULL DEFAULT 0
  CHECK (initial_pending IN (0, 1));
