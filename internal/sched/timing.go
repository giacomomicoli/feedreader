package sched

import (
	"math"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// maxIntervalSec keeps a per-feed interval from overflowing time.Duration.
const maxIntervalSec = math.MaxInt64 / int64(time.Second)

// DefaultInterval is the poll interval of a feed of the given kind that has
// no interval of its own: cfg.PollIntervalYouTube for YouTube feeds,
// cfg.PollInterval for all others. A non-positive setting falls back to
// config.DefaultYouTubePollInterval or config.DefaultPollInterval.
//
// It is the one definition of a kind's default, used by the scheduler and
// shown on the feed settings page.
func DefaultInterval(cfg config.Config, kind store.Kind) time.Duration {
	if kind == store.KindYouTube {
		if cfg.PollIntervalYouTube > 0 {
			return cfg.PollIntervalYouTube
		}
		return config.DefaultYouTubePollInterval
	}
	if cfg.PollInterval > 0 {
		return cfg.PollInterval
	}
	return config.DefaultPollInterval
}

// baseInterval is the feed's poll interval before server hints: its own
// override when set, else the default for its kind (DefaultInterval), never
// below config.MinPollInterval.
func (s *Scheduler) baseInterval(f store.Feed) time.Duration {
	d := DefaultInterval(s.cfg, f.Kind)
	if f.IntervalSec > 0 {
		d = time.Duration(min(int64(f.IntervalSec), maxIntervalSec)) * time.Second
	}
	return max(d, config.MinPollInterval)
}

// withHints applies the server's freshness hints — RSS <ttl> and
// Cache-Control max-age — as a lower bound on interval. Each hint
// is capped at config.MaxBackoff so a feed cannot schedule itself out of
// existence. Zero hints are ignored.
func withHints(interval, ttl, maxAge time.Duration) time.Duration {
	for _, hint := range []time.Duration{ttl, maxAge} {
		if hint > 0 {
			interval = max(interval, min(hint, config.MaxBackoff))
		}
	}
	return interval
}

// ttlSec converts a document's <ttl> to the whole seconds stored with the
// feed (store.Feed.TTLSec). It is capped at config.MaxBackoff, which
// withHints would cap it to anyway.
func ttlSec(ttl time.Duration) int {
	return int(min(max(ttl, 0), config.MaxBackoff) / time.Second)
}

// storedTTL is the <ttl> stored with f by its last parsed document. A 304
// carries no document, so its schedule uses this one (<ttl> is a lower
// bound on the interval).
func storedTTL(f store.Feed) time.Duration {
	return time.Duration(min(max(f.TTLSec, 0), ttlSec(config.MaxBackoff))) * time.Second
}

// failureDelay is how long to wait after the errorCount-th consecutive
// failure: interval × 2^errorCount capped at config.MaxBackoff, but never
// less than interval itself, and at least Retry-After (also capped) when the
// server sent one.
func failureDelay(interval time.Duration, errorCount int, retryAfter time.Duration) time.Duration {
	backoff := interval
	for i := 0; i < errorCount && backoff > 0 && backoff < config.MaxBackoff; i++ {
		backoff *= 2
	}
	d := max(interval, min(backoff, config.MaxBackoff))
	if retryAfter > 0 {
		d = max(d, min(retryAfter, config.MaxBackoff))
	}
	return d
}

// staggerTimes spreads n start times evenly across window, starting at now:
// now, now + window/n, now + 2·window/n, …
func staggerTimes(now time.Time, n int, window time.Duration) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = now.Add(window / time.Duration(n) * time.Duration(i))
	}
	return out
}
