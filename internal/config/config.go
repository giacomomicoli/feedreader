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

// Manual refresh.
const (
	// RefreshCheckInterval is how often the card grid asks whether the
	// fetches a Refresh click started are done, to reload itself once they
	// are.
	RefreshCheckInterval = 2 * time.Second
	// RefreshMaxChecks bounds those checks (about a minute in all): the grid
	// is then reloaded with whatever has arrived.
	RefreshMaxChecks = 30
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
	// SummaryMaxChars is the visible-text length summaries are truncated to
	// in the card grid (the title's tooltip).
	SummaryMaxChars = 300
	// SummaryFullMaxChars bounds the visible text, line breaks included, of
	// the full summary in the entry drawer: YouTube's limit for a video
	// description, far more than an article's summary needs.
	SummaryFullMaxChars = 5000
	// TranscriptMaxRunes bounds the stored text of a video transcript:
	// about three hours of speech; a longer one is cut.
	TranscriptMaxRunes = 200_000
)

// Transcripts. YouTube has no public API for them: they
// are read the way its Android app does, which works from a home connection
// at a low rate and gets refused when requests come too fast. So the
// transcripts of new videos are fetched one at a time, slowly, and every
// pause errs long.
const (
	// TranscriptDelay is the pause between two transcript requests.
	TranscriptDelay = 10 * time.Second
	// TranscriptBlockedPause is the pause after YouTube refused a request
	// (rate limit, bot check), or the refusal's Retry-After when longer, up
	// to MaxBackoff. The video stays queued and the refusal does not count
	// as one of its attempts. It is also the pause after a
	// transcript could not be saved, so a database that cannot be written
	// does not turn into a stream of requests for the same video.
	TranscriptBlockedPause = 1 * time.Hour
	// TranscriptMaxAttempts is how many times a video's transcript is
	// requested when the requests fail for other reasons (network, an
	// answer whose format changed, a video YouTube does not play) before
	// the video is given up on.
	TranscriptMaxAttempts = 3
	// TranscriptRetryDelay is how long a video waits after its first failed
	// attempt, doubled after each further one and capped at MaxBackoff: the
	// attempts are spread over hours, so an outage of the home connection
	// or of YouTube does not use them all up. The other queued videos are
	// fetched meanwhile.
	TranscriptRetryDelay = 2 * time.Hour
	// TranscriptRecheckDelay is how long a video waits before it is checked
	// again when it has not aired yet (a live stream on air, an upcoming
	// premiere or stream), or when its first check found no captions:
	// YouTube makes the automatic captions of a new upload, or of a stream
	// that has just ended, some time after it is published. A second check
	// without captions is final.
	TranscriptRecheckDelay = 6 * time.Hour
	// TranscriptMaxQueued is how long after it was queued a video
	// that has not aired yet is still checked again; it is then given up
	// on, so a stream announced for months, or one that never ends, is not
	// checked forever.
	TranscriptMaxQueued = 7 * 24 * time.Hour
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
	// Transcripts allows fetching the transcripts of YouTube videos
	// (FR_TRANSCRIPTS, true by default): the server's kill switch. Set to
	// false, no transcript is ever requested and the UI cannot turn them
	// on. Allowed, they are fetched only while the UI's global setting
	// (off until turned on) and the source's own switch are on too. See
	// the Transcripts constants.
	Transcripts bool
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
		Transcripts:         true,
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
	if v := vals["FR_TRANSCRIPTS"]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("FR_TRANSCRIPTS: must be true or false"))
		} else {
			c.Transcripts = b
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
