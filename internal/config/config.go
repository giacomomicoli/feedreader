// Package config holds runtime configuration (environment variables and an
// optional KEY=VALUE config file) and every tunable number of the reader.
// Other packages must use these constants instead of hardcoding values.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Application identity, used in the User-Agent.
const (
	AppName = "feedreader"
	RepoURL = "https://github.com/giacomomicoli/feedreader"
)

// Version is overridden at build time:
//
//	-ldflags "-X github.com/giacomomicoli/feedreader/internal/config.Version=v1.0.0"
var Version = "dev"

// Add flow and pagination.
const (
	// InitialUnread is how many of the newest entries are marked unread when a
	// feed is added; the rest are stored as read (archive).
	InitialUnread = 5
	// FeedPageSize is the "Load more" block size in single-feed scope.
	FeedPageSize = 5
	// ScopePageSize is the "Load more" block size in folder, All and the
	// other multi-feed scopes.
	ScopePageSize = 20
)

// Polling.
const (
	// DefaultPollInterval is the per-feed fetch interval unless overridden.
	DefaultPollInterval = 6 * time.Hour
	// DefaultYouTubePollInterval is the fetch interval of YouTube feeds
	// unless overridden. YouTube serves its feeds from a cache kept for 15
	// minutes (Cache-Control: max-age=900) and they only list the latest 15
	// videos, so polling them more often than blogs stays cheap.
	DefaultYouTubePollInterval = 1 * time.Hour
	// FirstRetryDelay is how soon a feed is fetched again after a single
	// failure, or one interval when that is shorter. Servers answer the odd
	// one-off error (YouTube sometimes returns 404 for a working channel),
	// so backoff only starts from the second failure in a row.
	FirstRetryDelay = 15 * time.Minute
	// MaxBackoff caps failure backoff (interval × 2^(error_count-1) from the
	// second failure in a row). It also caps server-supplied hints (RSS
	// <ttl>, Cache-Control max-age, Retry-After) so a misbehaving feed cannot
	// stop itself from ever being polled again.
	MaxBackoff = 24 * time.Hour
	// WarnAfterFailures is the number of consecutive failures after which the
	// sidebar shows a warning icon on the feed.
	WarnAfterFailures = 3
	// DefaultFetchWorkers is the maximum number of concurrent fetches.
	DefaultFetchWorkers = 4
	// MaxFetchWorkers is the largest accepted FR_FETCH_WORKERS, so a typo
	// fails as a configuration error instead of exhausting memory.
	MaxFetchWorkers = 64
	// StartupStagger is the window over which overdue feeds are spread when
	// the process starts, so they do not all fire at once.
	StartupStagger = 5 * time.Minute
	// MinPollInterval is the smallest accepted interval (global or per feed).
	MinPollInterval = 5 * time.Minute
)

// HTTP fetching.
const (
	// DefaultMaxBodyBytes caps response bodies (after decompression).
	DefaultMaxBodyBytes = 10 << 20
	// MaxRedirects: a redirect loop or more than this many hops is an error.
	MaxRedirects = 5
	// FetchTimeout bounds a single HTTP request, including reading the body.
	FetchTimeout = 30 * time.Second
)

// Content handling.
const (
	// SummaryMaxChars is the visible-text length summaries are truncated to.
	SummaryMaxChars = 300
)

// Config is the runtime configuration.
type Config struct {
	Listen              string        // FR_LISTEN
	DataDir             string        // FR_DATA_DIR
	PollInterval        time.Duration // FR_POLL_INTERVAL
	PollIntervalYouTube time.Duration // FR_POLL_INTERVAL_YOUTUBE (YouTube feeds)
	FetchWorkers        int           // FR_FETCH_WORKERS
	MaxBodyBytes        int64         // FR_FETCH_MAX_BODY
	UserAgent           string        // FR_USER_AGENT
	LogLevel            slog.Level    // FR_LOG_LEVEL (debug|info|warn|error)
	// AllowedHosts are extra host names the UI answers to (FR_ALLOWED_HOSTS,
	// comma-separated), e.g. the reverse proxy's site name.
	AllowedHosts []string
}

// DefaultUserAgent returns "<app-name>/<version> (+<repo-url>)".
func DefaultUserAgent() string {
	return AppName + "/" + Version + " (+" + RepoURL + ")"
}

// Default returns the configuration used when nothing is set.
func Default() Config {
	return Config{
		Listen:              "127.0.0.1:8080",
		DataDir:             "./data",
		PollInterval:        DefaultPollInterval,
		PollIntervalYouTube: DefaultYouTubePollInterval,
		FetchWorkers:        DefaultFetchWorkers,
		MaxBodyBytes:        DefaultMaxBodyBytes,
		UserAgent:           DefaultUserAgent(),
		LogLevel:            slog.LevelInfo,
	}
}

// DBPath is the SQLite database file inside DataDir.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "feedreader.db") }

// Load builds the configuration from defaults, then the optional config file
// at path (same KEY=VALUE format as .env.example and systemd EnvironmentFile),
// then the process environment, which wins.
func Load(path string) (Config, error) {
	vals := map[string]string{}
	if path != "" {
		fileVals, err := readEnvFile(path)
		if err != nil {
			return Config{}, err
		}
		vals = fileVals
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, "FR_") {
			vals[k] = v
		}
	}
	return fromMap(vals)
}

func fromMap(vals map[string]string) (Config, error) {
	c := Default()
	var errs []error
	if v := vals["FR_LISTEN"]; v != "" {
		c.Listen = v
	}
	if v := vals["FR_DATA_DIR"]; v != "" {
		c.DataDir = v
	}
	if err := parsePollInterval(vals, "FR_POLL_INTERVAL", &c.PollInterval); err != nil {
		errs = append(errs, err)
	}
	if err := parsePollInterval(vals, "FR_POLL_INTERVAL_YOUTUBE", &c.PollIntervalYouTube); err != nil {
		errs = append(errs, err)
	}
	if v := vals["FR_FETCH_WORKERS"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxFetchWorkers {
			errs = append(errs, fmt.Errorf("FR_FETCH_WORKERS: must be between 1 and %d", MaxFetchWorkers))
		} else {
			c.FetchWorkers = n
		}
	}
	if v := vals["FR_FETCH_MAX_BODY"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("FR_FETCH_MAX_BODY: must be a positive number of bytes"))
		} else {
			c.MaxBodyBytes = n
		}
	}
	if v := vals["FR_USER_AGENT"]; v != "" {
		c.UserAgent = v
	}
	if v := vals["FR_LOG_LEVEL"]; v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("FR_LOG_LEVEL: %w", err))
		}
	}
	for h := range strings.SplitSeq(vals["FR_ALLOWED_HOSTS"], ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// parsePollInterval sets *dst to the poll interval in vals[key], a Go duration of
// at least MinPollInterval. An empty value leaves *dst alone.
func parsePollInterval(vals map[string]string, key string, dst *time.Duration) error {
	v := vals[key]
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", key, err)
	case d < MinPollInterval:
		return fmt.Errorf("%s: must be at least %s", key, MinPollInterval)
	}
	*dst = d
	return nil
}

// readEnvFile parses KEY=VALUE lines; blank lines and # comments are skipped,
// and a value may be wrapped in single or double quotes.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config file %s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vals[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return vals, nil
}
