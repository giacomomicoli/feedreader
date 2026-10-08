package fetch

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"120", 2 * time.Minute},
		{"  30 ", 30 * time.Second},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
		{"Thursday, 08-Oct-26 12:01:00 GMT", time.Minute}, // RFC 850
		{"Thu Oct  8 12:02:00 2026", 2 * time.Minute},     // ANSI C asctime
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{now.Format(http.TimeFormat), 0},
		{"-5", 0},
		{"+5", 0},
		{"1.5", 0},
		{"5 seconds", 0},
		{"tomorrow", 0},
		{"99999999999999999999999", maxDeltaSeconds * time.Second},
		{"Fri, 31 Dec 9999 23:59:59 GMT", maxDeltaSeconds * time.Second},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseMaxAge(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  time.Duration
	}{
		{"absent", nil, 0},
		{"empty", []string{""}, 0},
		{"plain", []string{"max-age=300"}, 5 * time.Minute},
		{"with other directives", []string{"public, max-age=60, must-revalidate"}, time.Minute},
		{"case-insensitive name", []string{"Public, MAX-AGE=60"}, time.Minute},
		{"quoted argument", []string{`max-age="90"`}, 90 * time.Second},
		{"spaces around", []string{" max-age = 45 "}, 45 * time.Second},
		{"split across field lines", []string{"public", "max-age=3600"}, time.Hour},
		{"first occurrence wins", []string{"max-age=60, max-age=120"}, time.Minute},
		{"zero", []string{"max-age=0"}, 0},
		{"negative is invalid", []string{"max-age=-1"}, 0},
		{"non-numeric is invalid", []string{"max-age=abc"}, 0},
		{"fraction is invalid", []string{"max-age=1.5"}, 0},
		{"missing argument", []string{"max-age"}, 0},
		{"huge saturates", []string{"max-age=99999999999999999999"}, maxDeltaSeconds * time.Second},
		{"s-maxage is for shared caches", []string{"s-maxage=600"}, 0},
		{"no-cache only", []string{"no-cache"}, 0},
		{"no-store only", []string{"no-store"}, 0},
		{"no-store with max-age", []string{"no-store, max-age=600"}, 0},
		{"no-cache with max-age", []string{"max-age=600", "no-cache"}, 0},
		{"qualified no-cache keeps max-age", []string{`no-cache="Set-Cookie", max-age=600`}, 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMaxAge(tt.lines); got != tt.want {
				t.Errorf("parseMaxAge(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}
