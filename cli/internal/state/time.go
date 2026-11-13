package state

import (
	"encoding/json"
	"time"
)

// Time is a timestamp written as RFC 3339 in UTC with whole seconds
// ("2026-11-13T17:42:10Z"). Knock.app's decoder rejects fractional seconds,
// so every timestamp in ~/.knock uses this type.
type Time struct{ time.Time }

// NewTime truncates t to the second, in UTC.
func NewTime(t time.Time) Time { return Time{t.UTC().Truncate(time.Second)} }

// Now is NewTime(time.Now()).
func Now() Time { return NewTime(time.Now()) }

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte(`null`), nil
	}
	return json.Marshal(t.UTC().Truncate(time.Second).Format(time.RFC3339))
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	*t = NewTime(v)
	return nil
}
