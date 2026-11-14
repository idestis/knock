package termio

import (
	"slices"
	"strings"
	"testing"

	"github.com/idestis/knock/protocol"
)

func TestKeyReaderTokens(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"ls\r", []string{"l", "s", "⏎"}},
		{"a\tb\x7f\x08", []string{"a", "⇥", "b", "⌫", "⌫"}},
		{"\x03\x04\x1a", []string{"⌃C", "⌃D", "⌃Z"}},
		{"\x1b", []string{"⎋"}},
		{"\x1b[A\x1b[B\x1b[C\x1b[D\x1bOA", []string{"↑", "↓", "→", "←", "↑"}},
		{"\x1b[1;5C", []string{"→"}},
		{"\x1b[3~\x1b[Z", []string{"⌦", "⇤"}},
		{"\x1bb", []string{"⎋b"}},
		{"\x1b[15~", []string{"⎋15~"}},
		// Enter inside a bracketed paste still shows; the markers don't.
		{"\x1b[200~echo hi\rrm -rf x\r\x1b[201~", strings.Split("echo hi⏎rm -rf x⏎", "")},
		{"\r\n", []string{"⏎"}},
		{"héllo", []string{"h", "é", "l", "l", "o"}},
		// Bidi overrides, zero-width joiners and C1 controls never show.
		{"a\u202eb\u200dc\u0085d", []string{"a", "b", "c", "d"}},
		{"\xff", []string{"�"}},
	}
	for _, tt := range tests {
		var k KeyReader
		if got := k.Feed([]byte(tt.in)); !slices.Equal(got, tt.want) {
			t.Errorf("Feed(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKeyReaderSplitAcrossReads(t *testing.T) {
	var k KeyReader
	var got []string
	for _, part := range []string{"\x1b[", "A", "\xc3", "\xa9", "\x1b[20", "0~x\x1b[201~"} {
		got = append(got, k.Feed([]byte(part))...)
	}
	if want := []string{"↑", "é", "x"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestKeyTokensAreSafeToPrint(t *testing.T) {
	var k KeyReader
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	for _, tok := range k.Feed(all) {
		for _, r := range tok {
			if !visibleRune(r) {
				t.Fatalf("token %q holds %U", tok, r)
			}
		}
	}
}

func TestKeyRibbonKeepsTheLastKeys(t *testing.T) {
	r := KeyRibbon{Max: 4}
	r.Add([]string{"a", "b", "c"})
	r.Add([]string{"d", "⏎"})
	if got := r.String(); got != "bcd⏎" {
		t.Fatalf("got %q", got)
	}
}

func TestCleanText(t *testing.T) {
	tests := map[string]string{
		"Maya":                       "Maya",
		"Ma\x1b]52;c;ZXZpbA==\x07ya": "Ma]52;c;ZXZpbA==ya",
		"\x1b[2J\x1b[31mEve":         "[2J[31mEve",
		"evil\u202egnp.exe":          "evilgnp.exe",
		"zero\u200bwidth\ufeff":      "zerowidth",
		"line\u2028break":            "linebreak",
		"  padded  ":                 "padded",
	}
	for in, want := range tests {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanText(strings.Repeat("x", 200)); len([]rune(got)) != maxNameRunes+1 {
		t.Errorf("long names should be cut to %d runes plus an ellipsis, got %d", maxNameRunes, len([]rune(got)))
	}
	id := CleanIdentity(protocol.Identity{Name: "S\x1bam", Email: "s\u202e@x", Login: "l\x00", Provider: "github\x07"})
	if id.Name != "Sam" || id.Email != "s@x" || id.Login != "l" || id.Provider != "github" {
		t.Fatalf("CleanIdentity = %+v", id)
	}
}
