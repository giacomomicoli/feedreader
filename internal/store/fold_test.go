package store

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// orbit returns the runes strings.EqualFold treats as equal to r.
func orbit(r rune) []rune {
	out := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		out = append(out, f)
	}
	return out
}

func TestFoldRune_OneMemberOfEachCaseFoldingOrbit(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		k := foldRune(r)
		members := orbit(r)
		found := false
		for _, m := range members {
			if m == k {
				found = true
			}
			if got := foldRune(m); got != k {
				t.Fatalf("foldRune(%U) = %U but foldRune(%U) = %U: same orbit, different keys", r, k, m, got)
			}
		}
		if !found {
			t.Fatalf("foldRune(%U) = %U, which is not in its orbit %U", r, k, members)
		}
		if 'A' <= k && k <= 'Z' {
			t.Fatalf("foldRune(%U) = %U: keys must hold no ASCII upper case (SuggestTags uses LIKE)", r, k)
		}
	}
}

func TestFoldName_SameKeyExactlyWhenEqualFold(t *testing.T) {
	names := []string{
		"Città", "CITTÀ", "città", "Citta",
		"über", "ÜBER", "Über", "uber",
		"Économie", "économie", "ÉCONOMIE", "Economie",
		"straße", "STRASSE", "STRAẞE",
		"Σίσυφος", "ΣΊΣΥΦΟΣ", "σίσυφοσ",
		"İstanbul", "istanbul", "ISTANBUL",
		"Kelvin", "Kelvin", "ſail", "Sail",
		"Go", "GO", "go ", "",
	}
	for _, a := range names {
		for _, b := range names {
			if same, want := foldName(a) == foldName(b), strings.EqualFold(a, b); same != want {
				t.Errorf("foldName(%q) == foldName(%q) is %v; strings.EqualFold says %v", a, b, same, want)
			}
		}
	}
	if got := foldName("Città CITTÀ"); got != "città città" {
		t.Errorf("foldName = %q, want lower case %q", got, "città città")
	}
}
