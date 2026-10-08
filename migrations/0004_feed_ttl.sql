-- RSS 2.0 <ttl> of the feed's last parsed document, in seconds (0 = none). It
-- is a lower bound on the poll interval, but a 304 Not Modified has no body to
-- read it from: the scheduler applies the stored value instead, so a feed is
-- not polled more often than its <ttl> allows after the first 304.
ALTER TABLE feeds ADD COLUMN ttl_sec INTEGER NOT NULL DEFAULT 0;
