package innertube

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPlayerRequest(t *testing.T) {
	r := PlayerRequest(Android, "aaaaaaaaaaa")
	if r.Method != http.MethodPost || r.URL != PlayerURL || r.ContentType != JSONType || r.Accept != JSONType ||
		r.ETag != "" || r.LastModified != "" || r.NoRedirects {
		t.Fatalf("request = %+v", r)
	}
	want := fmt.Sprintf(`{"context":{"client":{"clientName":%q,"clientVersion":%q,"hl":%q}},"videoId":"aaaaaaaaaaa"}`,
		AndroidName, AndroidVersion, Lang)
	if r.Body != want {
		t.Fatalf("body = %s, want %s", r.Body, want)
	}
}

func TestBlockedStatus(t *testing.T) {
	for code, want := range map[int]bool{
		http.StatusTooManyRequests: true, http.StatusForbidden: true, http.StatusUnauthorized: true,
		http.StatusFound: true, http.StatusPermanentRedirect: true, http.StatusMultipleChoices: true,
		http.StatusOK: false, http.StatusBadRequest: false, http.StatusNotFound: false, http.StatusInternalServerError: false,
	} {
		if got := BlockedStatus(code); got != want {
			t.Errorf("BlockedStatus(%d) = %v", code, got)
		}
	}
}

func TestContentTypes(t *testing.T) {
	if !IsWebPage("text/html; charset=utf-8") || IsWebPage(JSONType) || IsWebPage("") {
		t.Error("IsWebPage")
	}
	if !IsJSON("application/json; charset=UTF-8") || IsJSON("text/html") || IsJSON("application/jsonx") {
		t.Error("IsJSON")
	}
}

func TestClassify(t *testing.T) {
	for _, tt := range []struct {
		status, reason string
		want           Playability
	}{
		{"OK", "", Playable},
		{"LOGIN_REQUIRED", "Sign in to confirm you're not a bot", Refused},
		{"LOGIN_REQUIRED", "", Refused},
		{"LOGIN_REQUIRED", "Sign in to confirm your age", Unavailable},
		{"LOGIN_REQUIRED", "This video is private", Unavailable},
		{"ERROR", "This video is unavailable", Unavailable},
		{"UNPLAYABLE", "", Unavailable},
		{"LIVE_STREAM_OFFLINE", "", NotReady},
		{"SOMETHING_NEW", "", Unknown},
		{"", "", Unknown},
	} {
		if got := Classify(tt.status, tt.reason); got != tt.want {
			t.Errorf("Classify(%q, %q) = %v, want %v", tt.status, tt.reason, got, tt.want)
		}
	}
}

func TestStatusToken(t *testing.T) {
	for s, want := range map[string]bool{
		"OK": true, "LIVE_STREAM_OFFLINE": true, strings.Repeat("A", MaxStatusBytes): true,
		strings.Repeat("A", MaxStatusBytes+1): false, "": false, "ok": false, "<b>": false, "A B": false,
	} {
		if got := StatusToken(s); got != want {
			t.Errorf("StatusToken(%q) = %v", s, got)
		}
	}
}
