package app

import (
	"slices"
	"testing"
)

func TestExtractRelay(t *testing.T) {
	tests := []struct {
		in    []string
		relay string
		rest  []string
	}{
		{[]string{"--relay", "https://r.example", "open"}, "https://r.example", []string{"open"}},
		{[]string{"open", "--relay=https://r.example", "--tmux"}, "https://r.example", []string{"open", "--tmux"}},
		{[]string{"k7f3x9q2", "4821KQ"}, "", []string{"k7f3x9q2", "4821KQ"}},
		{[]string{"--", "--relay"}, "", []string{"--relay"}},
	}
	for _, tt := range tests {
		relay, rest, err := extractRelay(tt.in)
		if err != nil || relay != tt.relay || !slices.Equal(rest, tt.rest) {
			t.Errorf("extractRelay(%q) = %q %q %v", tt.in, relay, rest, err)
		}
	}
	if _, _, err := extractRelay([]string{"open", "--relay"}); err == nil {
		t.Error("--relay without a value should fail")
	}
}

func TestSentence(t *testing.T) {
	if got := sentence("can't reach the relay"); got != "Can't reach the relay." {
		t.Fatalf("got %q", got)
	}
}
