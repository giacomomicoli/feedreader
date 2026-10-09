package safeurl

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactedHidesAllUserinfo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://site.example/feed.xml?x=1#f", "https://site.example/feed.xml?x=1#f"},
		{"https://tok3n@site.example/feed.xml", "https://" + Placeholder + "@site.example/feed.xml"},
		{"https://reader:s3cret@site.example/feed.xml", "https://" + Placeholder + "@site.example/feed.xml"},
		{"https://reader:@site.example/feed.xml", "https://" + Placeholder + "@site.example/feed.xml"},
		{"https://:s3cret@site.example/feed.xml", "https://" + Placeholder + "@site.example/feed.xml"},
		{"http://a%40b:p%3Aw@site.example:8080/x", "http://" + Placeholder + "@site.example:8080/x"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := Redacted(u); got != tc.want {
			t.Errorf("Redacted(%s) = %s; want %s", tc.in, got, tc.want)
		}
		if got := String(tc.in); got != tc.want {
			t.Errorf("String(%s) = %s; want %s", tc.in, got, tc.want)
		}
		if u.User != nil && u.String() != tc.in {
			t.Errorf("Redacted changed its argument to %s", u.String())
		}
	}
}

func TestRedactedEdgeCases(t *testing.T) {
	if got := Redacted(nil); got != "" {
		t.Errorf("Redacted(nil) = %q", got)
	}
	if got := String("https://tok3n@[::1"); got != Invalid || strings.Contains(got, "tok3n") {
		t.Errorf("String(unparsable) = %q; want %q", got, Invalid)
	}
	// An opaque URL has no parsed userinfo, but its opaque part may hold
	// credentials.
	if got := String("https:tok3n@host.example/feed"); got != "https:"+Placeholder {
		t.Errorf("String(opaque) = %q; want %q", got, "https:"+Placeholder)
	}
}
