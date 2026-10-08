package parse

import (
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name, body, contentType string
		want                    bool
	}{
		{"RSS 2.0", `<?xml version="1.0"?><rss version="2.0"><channel/></rss>`, "application/rss+xml", true},
		{"RSS 0.91 uppercase-insensitive", `<RSS version="0.91"><channel/></RSS>`, "", true},
		{"RSS 1.0 RDF", `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/"><channel/></rdf:RDF>`, "", true},
		{"Atom", `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title></feed>`, "application/atom+xml", true},
		{"Atom with prefixed root", `<atom:feed xmlns:atom="http://www.w3.org/2005/Atom"/>`, "", true},
		{"RSS served as text/html", `<rss version="2.0"><channel/></rss>`, "text/html", true},
		{"BOM, whitespace, comments, PI and doctype before root",
			"\xEF\xBB\xBF \n<?xml version=\"1.0\"?>\n<!-- generator: x -->\n<?xml-stylesheet href=\"s.xsl\"?>\n<!DOCTYPE rss [<!ENTITY t \"a>b\">]>\n<rss/>", "", true},
		{"UTF-16 BOM", string(utf16Bytes(`<?xml version="1.0" encoding="UTF-16"?><rss/>`, true)), "", true},
		{"root beyond 4 KiB of comments", "<!--" + strings.Repeat("x", 8192) + "--><feed/>", "", true},
		{"JSON Feed", `{"version":"https://jsonfeed.org/version/1.1","title":"x","items":[]}`, "application/feed+json", true},
		{"JSON Feed 1.0, version last", `  {"title":"x","items":[{"id":"1"}],"version":"https://jsonfeed.org/version/1"}`, "application/json", true},
		{"JSON Feed with BOM", "\xEF\xBB\xBF{\"version\":\"https://jsonfeed.org/version/1.1\"}", "", true},
		{"JSON without version, feed media type hint", `{"title":"x","items":[]}`, "application/feed+json; charset=utf-8", true},

		{"HTML page", `<!DOCTYPE html><html><head><link rel="alternate" type="application/rss+xml" href="/feed"></head></html>`, "text/html", false},
		{"HTML with feed media type", `<html><body><rss/></body></html>`, "application/rss+xml", false},
		{"XHTML with XML declaration", `<?xml version="1.0"?><!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "x"><html xmlns="http://www.w3.org/1999/xhtml"/>`, "application/xhtml+xml", false},
		{"other XML", `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"/>`, "application/xml", false},
		{"element whose name starts with feed", `<feedback/>`, "", false},
		{"JSON without version", `{"title":"x","items":[]}`, "application/json", false},
		{"JSON with other version", `{"version":"1.1","items":[]}`, "application/feed+json", true},
		{"JSON hint but no items", `{"title":"x"}`, "application/feed+json", false},
		{"JSON array", `[1,2,3]`, "application/feed+json", false},
		{"invalid JSON", `{"version":`, "application/feed+json", false},
		{"empty", "", "application/rss+xml", false},
		{"whitespace", "   \n", "", false},
		{"plain text", "rss feed", "text/plain", false},
		{"lone angle bracket", "<", "", false},
		{"unterminated comment", "<!-- <rss>", "", false},
		{"unterminated PI", "<?xml <rss>", "", false},
		{"binary", "\x00\x01\x02<rss>", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Detect([]byte(c.body), c.contentType); got != c.want {
				t.Errorf("Detect = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDetect_FixturesAgreeWithParse(t *testing.T) {
	for _, name := range []string{"rss2_content.xml", "atom_xmlbase.xml", "jsonfeed11.json", "youtube.xml", "iso8859_1.xml", "malformed.xml"} {
		if !Detect(readFixture(t, name), "") {
			t.Errorf("Detect(%s) = false", name)
		}
	}
	if Detect(readFixture(t, "html_page.html"), "application/rss+xml") {
		t.Error("Detect(html_page.html) = true")
	}
}
