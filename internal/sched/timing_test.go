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

func TestFailureBackoff_IntervalTimesTwoToTheErrorCountCappedAtMaxBackoff(t *testing.T) {
	tests := []struct {
		name       string
		interval   time.Duration
		errorCount int
		retryAfter time.Duration
		want       time.Duration
	}{
		{"first failure doubles the interval", quarterCap, 1, 0, 2 * quarterCap},
		{"second failure reaches the cap", quarterCap, 2, 0, maxBackoff},
		{"third failure stays at the cap", quarterCap, 3, 0, maxBackoff},
		{"huge error count does not overflow", quarterCap, 1 << 30, 0, maxBackoff},
		{"short interval grows exponentially", shortIvl, 3, 0, 8 * shortIvl},
		{"short interval is capped", shortIvl, 6, 0, maxBackoff},
		{"minimum interval", config.MinPollInterval, 1, 0, 2 * config.MinPollInterval},
		{"interval above the cap is never shortened", 2 * maxBackoff, 2, 0, 2 * maxBackoff},
		{"no failure yet is one interval", quarterCap, 0, 0, quarterCap},
		{"Retry-After longer than the backoff wins", shortIvl, 1, quarterCap, quarterCap},
		{"Retry-After shorter than the backoff is a lower bound only", quarterCap, 1, shortIvl, 2 * quarterCap},
		{"Retry-After is capped", shortIvl, 1, 3 * maxBackoff, maxBackoff},
		{"Retry-After cannot shorten a long interval", 2 * maxBackoff, 1, 3 * maxBackoff, 2 * maxBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureDelay(tt.interval, tt.errorCount, tt.retryAfter); got != tt.want {
				t.Errorf("failureDelay(%s, %d, %s) = %s, want %s", tt.interval, tt.errorCount, tt.retryAfter, got, tt.want)
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
