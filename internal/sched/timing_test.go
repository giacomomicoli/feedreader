package sched

import (
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

const hour = time.Hour

// The timing tables use intervals relative to the cap, so they hold for any
// config.MaxBackoff.
const (
	maxBackoff = config.MaxBackoff
	quarterCap = config.MaxBackoff / 4 // reaches the cap on the second failure
	shortIvl   = config.MaxBackoff / 32
)

func TestFailureDelay_FirstFailureRetriesSoonThenBacksOffExponentially(t *testing.T) {
	tests := []struct {
		name       string
		interval   time.Duration
		errorCount int
		retryAfter time.Duration
		want       time.Duration
	}{
		{"first failure retries after the first retry delay", quarterCap, 1, 0, config.FirstRetryDelay},
		{"first failure of a shorter interval retries after one interval", config.MinPollInterval, 1, 0, config.MinPollInterval},
		{"first failure of a long interval still retries soon", 2 * maxBackoff, 1, 0, config.FirstRetryDelay},
		{"second failure doubles the interval", quarterCap, 2, 0, 2 * quarterCap},
		{"third failure reaches the cap", quarterCap, 3, 0, maxBackoff},
		{"fourth failure stays at the cap", quarterCap, 4, 0, maxBackoff},
		{"huge error count does not overflow", quarterCap, 1 << 30, 0, maxBackoff},
		{"short interval grows exponentially", shortIvl, 4, 0, 8 * shortIvl},
		{"short interval is capped", shortIvl, 7, 0, maxBackoff},
		{"minimum interval", config.MinPollInterval, 2, 0, 2 * config.MinPollInterval},
		{"interval above the cap is never shortened", 2 * maxBackoff, 2, 0, 2 * maxBackoff},
		{"no failure yet is one interval", quarterCap, 0, 0, quarterCap},
		{"negative error count is one interval", quarterCap, -1, 0, quarterCap},
		{"Retry-After longer than the first retry wins", quarterCap, 1, 2 * config.FirstRetryDelay, 2 * config.FirstRetryDelay},
		{"Retry-After shorter than the first retry is a lower bound only", quarterCap, 1, config.FirstRetryDelay / 2, config.FirstRetryDelay},
		{"Retry-After on a first failure is capped", shortIvl, 1, 3 * maxBackoff, maxBackoff},
		{"Retry-After longer than the backoff wins", shortIvl, 2, quarterCap, quarterCap},
		{"Retry-After shorter than the backoff is a lower bound only", quarterCap, 2, shortIvl, 2 * quarterCap},
		{"Retry-After is capped", shortIvl, 2, 3 * maxBackoff, maxBackoff},
		{"Retry-After cannot shorten a long interval", 2 * maxBackoff, 2, 3 * maxBackoff, 2 * maxBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureDelay(tt.interval, tt.errorCount, tt.retryAfter); got != tt.want {
				t.Errorf("failureDelay(%s, %d, %s) = %s, want %s", tt.interval, tt.errorCount, tt.retryAfter, got, tt.want)
			}
		})
	}
}

// TestFailureDelay_ConsecutiveFailuresOfTheDefaultIntervals: one failure is
// retried after config.FirstRetryDelay, a real outage still backs off from
// the second failure in a row (6h feed: 15m, 12h, 24h, 24h; 1h YouTube
// feed: 15m, 2h, 4h, 8h, 16h, 24h).
func TestFailureDelay_ConsecutiveFailuresOfTheDefaultIntervals(t *testing.T) {
	blog, yt := config.DefaultPollInterval, config.DefaultYouTubePollInterval
	tests := []struct {
		name     string
		interval time.Duration
		want     []time.Duration // after failure 1, 2, …
	}{
		{"default interval", blog, []time.Duration{config.FirstRetryDelay, 2 * blog, maxBackoff, maxBackoff}},
		{"YouTube interval", yt, []time.Duration{config.FirstRetryDelay, 2 * yt, 4 * yt, 8 * yt, 16 * yt, maxBackoff, maxBackoff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, want := range tt.want {
				if got := failureDelay(tt.interval, i+1, 0); got != want {
					t.Errorf("failure %d of a %s feed: retry in %s, want %s", i+1, tt.interval, got, want)
				}
			}
		})
	}
}

func TestIntervalHints_TTLAndMaxAgeAreLowerBoundsCappedAtMaxBackoff(t *testing.T) {
	tests := []struct {
		name               string
		interval, ttl, age time.Duration
		want               time.Duration
	}{
		{"no hints", quarterCap, 0, 0, quarterCap},
		{"shorter ttl does not speed polling up", quarterCap, shortIvl, 0, quarterCap},
		{"longer ttl slows polling down", quarterCap, 2 * quarterCap, 0, 2 * quarterCap},
		{"longer max-age slows polling down", quarterCap, 0, 3 * quarterCap, 3 * quarterCap},
		{"the larger hint wins", quarterCap, 2 * quarterCap, 3 * quarterCap, 3 * quarterCap},
		{"ttl is capped", quarterCap, 7 * maxBackoff, 0, maxBackoff},
		{"max-age is capped", quarterCap, 0, 40 * maxBackoff, maxBackoff},
		{"a long per-feed interval is kept", 5 * quarterCap, 2 * maxBackoff, 0, 5 * quarterCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withHints(tt.interval, tt.ttl, tt.age); got != tt.want {
				t.Errorf("withHints(%s, %s, %s) = %s, want %s", tt.interval, tt.ttl, tt.age, got, tt.want)
			}
		})
	}
}

func TestBaseInterval_PerFeedOverrideElseDefaultOfItsKind(t *testing.T) {
	s := New(nil, nil, config.Default(), nil)
	tests := []struct {
		name        string
		kind        store.Kind
		intervalSec int
		want        time.Duration
	}{
		{"global default", store.KindRSS, 0, config.DefaultPollInterval},
		{"YouTube default", store.KindYouTube, 0, config.DefaultYouTubePollInterval},
		{"per-feed override", store.KindRSS, 3600, hour},
		{"per-feed override of a YouTube feed", store.KindYouTube, int(config.DefaultPollInterval / time.Second), config.DefaultPollInterval},
		{"never below the minimum", store.KindRSS, 1, config.MinPollInterval},
		{"YouTube never below the minimum", store.KindYouTube, 1, config.MinPollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.baseInterval(store.Feed{Kind: tt.kind, IntervalSec: tt.intervalSec}); got != tt.want {
				t.Errorf("baseInterval(%s, %d s) = %s, want %s", tt.kind, tt.intervalSec, got, tt.want)
			}
		})
	}
	if got := s.baseInterval(store.Feed{IntervalSec: int(^uint(0) >> 1)}); got <= 0 {
		t.Errorf("huge per-feed interval overflowed to %s", got)
	}

	cfg := config.Default()
	cfg.PollInterval = 2 * hour
	cfg.PollIntervalYouTube = 3 * config.MinPollInterval
	s = New(nil, nil, cfg, nil)
	if got := s.baseInterval(store.Feed{Kind: store.KindRSS}); got != cfg.PollInterval {
		t.Errorf("configured poll_interval: got %s, want %s", got, cfg.PollInterval)
	}
	if got := s.baseInterval(store.Feed{Kind: store.KindYouTube}); got != cfg.PollIntervalYouTube {
		t.Errorf("configured YouTube poll interval: got %s, want %s", got, cfg.PollIntervalYouTube)
	}
	cfg.PollInterval, cfg.PollIntervalYouTube, cfg.FetchWorkers = 0, 0, 0
	s = New(nil, nil, cfg, nil)
	if got := s.baseInterval(store.Feed{}); got != config.DefaultPollInterval {
		t.Errorf("zero poll_interval: got %s, want the default", got)
	}
	if got := s.baseInterval(store.Feed{Kind: store.KindYouTube}); got != config.DefaultYouTubePollInterval {
		t.Errorf("zero YouTube poll interval: got %s, want the default", got)
	}
	if s.cfg.PollInterval != config.DefaultPollInterval || s.cfg.PollIntervalYouTube != config.DefaultYouTubePollInterval {
		t.Errorf("New kept zero intervals: %s, %s", s.cfg.PollInterval, s.cfg.PollIntervalYouTube)
	}
	if s.cfg.FetchWorkers != config.DefaultFetchWorkers {
		t.Errorf("zero fetch workers: got %d, want the default", s.cfg.FetchWorkers)
	}
}

func TestDefaultInterval_ByKindWithFallbacks(t *testing.T) {
	custom := config.Default()
	custom.PollInterval = 2 * config.DefaultPollInterval
	custom.PollIntervalYouTube = 2 * config.DefaultYouTubePollInterval
	tests := []struct {
		name string
		cfg  config.Config
		kind store.Kind
		want time.Duration
	}{
		{"rss default", config.Default(), store.KindRSS, config.DefaultPollInterval},
		{"YouTube default", config.Default(), store.KindYouTube, config.DefaultYouTubePollInterval},
		{"configured rss", custom, store.KindRSS, custom.PollInterval},
		{"configured YouTube", custom, store.KindYouTube, custom.PollIntervalYouTube},
		{"unknown kind uses the global interval", custom, store.Kind(""), custom.PollInterval},
		{"unset rss falls back", config.Config{}, store.KindRSS, config.DefaultPollInterval},
		{"unset YouTube falls back", config.Config{}, store.KindYouTube, config.DefaultYouTubePollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultInterval(tt.cfg, tt.kind); got != tt.want {
				t.Errorf("DefaultInterval(%q) = %s, want %s", tt.kind, got, tt.want)
			}
		})
	}
}

func TestStaggerTimes_SpreadEvenlyAcrossTheWindow(t *testing.T) {
	got := staggerTimes(t0, 4, config.StartupStagger)
	step := config.StartupStagger / 4
	for i, at := range got {
		if want := t0.Add(time.Duration(i) * step); !at.Equal(want) {
			t.Errorf("slot %d = %s, want %s", i, at, want)
		}
	}
	if last := got[len(got)-1]; !last.Before(t0.Add(config.StartupStagger)) {
		t.Errorf("last slot %s is not inside the window", last)
	}
	if one := staggerTimes(t0, 1, config.StartupStagger); len(one) != 1 || !one[0].Equal(t0) {
		t.Errorf("single overdue feed: %v, want it due immediately", one)
	}
	if none := staggerTimes(t0, 0, config.StartupStagger); len(none) != 0 {
		t.Errorf("no overdue feeds: %v", none)
	}
}
