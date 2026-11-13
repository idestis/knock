package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecentJSONTimestamps(t *testing.T) {
	when := time.Date(2026, 11, 13, 18, 42, 10, 123456789, time.FixedZone("CET", 3600))
	ms := NewTime(when)
	r := Recent{Items: []RecentItem{
		{DoorID: "k7f3x9q2", LastSessionAt: NewTime(when)},
		{DoorID: "a2b3c4d5", LastSessionAt: NewTime(when), MismatchSince: &ms},
	}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"last_session_at":"2026-11-13T17:42:10Z"`) || strings.Contains(s, ".123") {
		t.Fatalf("timestamps not whole-second UTC: %s", s)
	}
	if strings.Count(s, "mismatch_since") != 1 {
		t.Fatalf("nil mismatch_since should be omitted: %s", s)
	}
	var back Recent
	if err := json.Unmarshal(b, &back); err != nil || !back.Items[1].MismatchSince.Equal(ms.Time) {
		t.Fatalf("round trip: %v", err)
	}
}
