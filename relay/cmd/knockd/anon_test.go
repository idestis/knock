package main

import "testing"

func TestAllowAnonymous(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setting   string
		providers int
		want      bool
		err       bool
	}{
		{"local dev, nothing set", "", 0, true, false},
		{"provider set, default requires sign-in", "", 1, false, false},
		{"two providers", "", 2, false, false},
		{"operator opts back in", "true", 2, true, false},
		{"explicit false", "false", 1, false, false},
		{"false without providers locks everyone out", "false", 0, false, true},
		{"garbage", "maybe", 1, false, true},
		{"spaces and case", " TRUE ", 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := allowAnonymous(tc.setting, tc.providers)
			if (err != nil) != tc.err || (err == nil && got != tc.want) {
				t.Fatalf("allowAnonymous(%q, %d) = %v, %v; want %v, err %v", tc.setting, tc.providers, got, err, tc.want, tc.err)
			}
		})
	}
}
