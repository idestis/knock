package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestBannerShape(t *testing.T) {
	b := Banner(0)
	rows := strings.Split(b, "\n")
	if len(rows) != (len(markGrid)+1)/2 {
		t.Fatalf("want %d rows, got %d", (len(markGrid)+1)/2, len(rows))
	}
	for _, r := range rows {
		if w := lipgloss.Width(r); w != len(markGrid[0]) {
			t.Fatalf("row width %d, want %d", w, len(markGrid[0]))
		}
	}
	if bannerColor(0) != "#9CC0FF" || bannerColor(1) != "#0043F3" {
		t.Fatalf("gradient ends: %s %s", bannerColor(0), bannerColor(1))
	}
	t.Log("\n" + b)
}
