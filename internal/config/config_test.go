package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	c, err := fromMap(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 6*time.Hour || c.FetchWorkers != 4 || c.MaxBodyBytes != 10<<20 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Fatalf("default listen should be loopback, got %q", c.Listen)
	}
}

// isolateEnv unsets every inherited FR_* variable for the duration of the
// test, so a developer's exported .env cannot leak into Load. It mirrors the
// helper in cmd/feedreader/main_test.go.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "FR_") {
			t.Setenv(k, "") // restores the original value at cleanup
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	content := "# comment\nFR_LISTEN=0.0.0.0:9000\nFR_POLL_INTERVAL=\"2h\"\nFR_LOG_LEVEL=debug\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateEnv(t)
	t.Setenv("FR_POLL_INTERVAL", "3h")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" {
		t.Errorf("listen from file: got %q", c.Listen)
	}
	if c.PollInterval != 3*time.Hour {
		t.Errorf("env should override file: got %s", c.PollInterval)
	}
	if c.LogLevel != slog.LevelDebug {
		t.Errorf("log level: got %s", c.LogLevel)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for _, m := range []map[string]string{
		{"FR_POLL_INTERVAL": "soon"},
		{"FR_POLL_INTERVAL": "1s"},
		{"FR_FETCH_WORKERS": "0"},
		{"FR_FETCH_WORKERS": "99999999999"},
		{"FR_FETCH_MAX_BODY": "-1"},
		{"FR_LOG_LEVEL": "loud"},
	} {
		if _, err := fromMap(m); err == nil {
			t.Errorf("expected error for %v", m)
		}
	}
}

func TestFetchWorkersBound(t *testing.T) {
	c, err := fromMap(map[string]string{"FR_FETCH_WORKERS": strconv.Itoa(MaxFetchWorkers)})
	if err != nil {
		t.Fatalf("MaxFetchWorkers should be accepted: %v", err)
	}
	if c.FetchWorkers != MaxFetchWorkers {
		t.Fatalf("got %d workers, want %d", c.FetchWorkers, MaxFetchWorkers)
	}
	if _, err := fromMap(map[string]string{"FR_FETCH_WORKERS": strconv.Itoa(MaxFetchWorkers + 1)}); err == nil {
		t.Fatal("expected error above MaxFetchWorkers")
	}
}

func TestAllowedHosts(t *testing.T) {
	for in, want := range map[string][]string{
		"":                                   nil,
		" , ":                                nil,
		"feeds.lan.example":                  {"feeds.lan.example"},
		" feeds.lan.example ,, reader.home ": {"feeds.lan.example", "reader.home"},
	} {
		c, err := fromMap(map[string]string{"FR_ALLOWED_HOSTS": in})
		if err != nil {
			t.Fatalf("FR_ALLOWED_HOSTS=%q: %v", in, err)
		}
		if !slices.Equal(c.AllowedHosts, want) {
			t.Errorf("FR_ALLOWED_HOSTS=%q: got %q, want %q", in, c.AllowedHosts, want)
		}
	}
}
