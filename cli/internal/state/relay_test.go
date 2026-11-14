package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://knock.example.com", "https://knock.example.com"},
		{"https://knock.example.com/", "https://knock.example.com"},
		{"https://knock.example.com///", "https://knock.example.com"},
		{"HTTPS://Knock.Example.COM", "https://knock.example.com"},
		{"https://knock.example.com:443", "https://knock.example.com"},
		{"https://knock.example.com:443/", "https://knock.example.com"},
		{"http://knock.example.com:80", "http://knock.example.com"},
		{"https://knock.example.com:80", "https://knock.example.com:80"},
		{"http://knock.example.com:443", "http://knock.example.com:443"},
		{"https://knock.example.com:", "https://knock.example.com"},
		{"http://127.0.0.1:8787", "http://127.0.0.1:8787"},
		{"http://127.0.0.1:08787", "http://127.0.0.1:8787"},
		{"https://knock.example.com/relay/path?x=1#frag", "https://knock.example.com"},
		{"https://user:pw@knock.example.com", "https://knock.example.com"},
		{"  https://knock.example.com  ", "https://knock.example.com"},
		{"http://[::1]:8787", "http://[::1]:8787"},
		{"http://[::1]", "http://[::1]"},
		{"http://[::1]:80/", "http://[::1]"},
		{"https://[2001:DB8::1]:8443/x", "https://[2001:db8::1]:8443"},
		{"http://127.0.0.1:8787/path", "http://127.0.0.1:8787"},
		{"http://localhost", "http://localhost"},
		// http and https on the same host are different relays.
		{"http://knock.example.com", "http://knock.example.com"},
	}
	for _, tt := range tests {
		got, err := Origin(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("Origin(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "knock.example.com", "ftp://knock.example.com", "https://", "https:///path",
		"https://knock.example.com:abc", "https://knock.example.com:0", "ws://knock.example.com", "file:///etc",
		"https://knöck.example.com", "https://xn--knck-0ra.example.com\u00e9", "https://kn%6Fck.example.com",
		"http://[fe80::1%25en0]:8787", "https://knock_example.com"} {
		if got, err := Origin(bad); err == nil {
			t.Errorf("Origin(%q) = %q, want an error", bad, got)
		}
	}
}

func TestFolderName(t *testing.T) {
	tests := []struct{ origin, want string }{
		{"https://knock.example.com", "https_knock.example.com"},
		{"http://127.0.0.1:8787", "http_127.0.0.1_8787"},
		{"http://[::1]:8787", "http_--1_8787"},
		{"http://[::1]", "http_--1"},
		{"https://[2001:db8::1]:8443", "https_2001-db8--1_8443"},
		{"https://knock.example.com:8443", "https_knock.example.com_8443"},
		{"http://localhost", "http_localhost"},
	}
	for _, tt := range tests {
		if got := FolderName(tt.origin); got != tt.want {
			t.Errorf("FolderName(%q) = %q, want %q", tt.origin, got, tt.want)
		}
	}
	// Different relays never share a folder; the same relay spelt
	// differently always does.
	a, _ := Origin("http://knock.example.com")
	b, _ := Origin("https://knock.example.com")
	c, _ := Origin("HTTPS://knock.example.com:443/")
	if FolderName(a) == FolderName(b) {
		t.Error("http and https must not share state")
	}
	if FolderName(b) != FolderName(c) {
		t.Error("the same relay spelt differently must share state")
	}
}

// The same inputs and outputs Knock.app's tests pin (mac/Tests).
func TestOriginParityWithApp(t *testing.T) {
	for _, tt := range []struct{ in, origin, folder string }{
		{"https://Knock.Example.com/", "https://knock.example.com", "https_knock.example.com"},
		{"https://knock.example.com:443", "https://knock.example.com", "https_knock.example.com"},
		{"http://127.0.0.1:8787/path", "http://127.0.0.1:8787", "http_127.0.0.1_8787"},
		{"http://[::1]:8787", "http://[::1]:8787", "http_--1_8787"},
		{"http://localhost", "http://localhost", "http_localhost"},
	} {
		o, err := Origin(tt.in)
		if err != nil || o != tt.origin || FolderName(o) != tt.folder {
			t.Errorf("%q -> %q (%v), %q; want %q, %q", tt.in, o, err, FolderName(o), tt.origin, tt.folder)
		}
	}
}

// Knock.app may leave a just-migrated door.json without relay, or with a
// null code_created_at: both are this relay's door.
func TestDoorWithoutRelayField(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	s, _ := ForRelay("https://a.example")
	if err := os.MkdirAll(s.Dir, DirMode); err != nil {
		t.Fatal(err)
	}
	body := `{"door_id":"bvjy","door_key":"key","code":"4821KQ","code_version":2,"code_created_at":null,` +
		`"salt":"AAAAAAAAAAAAAAAAAAAAAA==","code_hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`
	if err := os.WriteFile(s.DoorPath(), []byte(body), FileMode); err != nil {
		t.Fatal(err)
	}
	d, err := s.LoadOrCreateDoor()
	if err != nil || d.DoorKey != "key" || d.Code != "4821KQ" {
		t.Fatalf("door without relay not accepted: %+v %v", d, err)
	}
	b, _ := os.ReadFile(s.DoorPath())
	if !strings.Contains(string(b), `"relay": "https://a.example"`) {
		t.Fatalf("relay not stamped: %s", b)
	}
	var raw struct {
		CodeCreatedAt string `json:"code_created_at"`
	}
	_ = json.Unmarshal(b, &raw)
	if _, err := time.Parse(time.RFC3339, raw.CodeCreatedAt); err != nil || strings.Contains(raw.CodeCreatedAt, ".") || !strings.HasSuffix(raw.CodeCreatedAt, "Z") {
		t.Fatalf("code_created_at %q is not whole-second RFC 3339 UTC", raw.CodeCreatedAt)
	}
}

func TestForRelay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOCK_HOME", home)
	s, err := ForRelay("https://Knock.Example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "https://knock.example.com" || s.Dir != filepath.Join(home, "relays", "https_knock.example.com") {
		t.Fatalf("store = %+v", s)
	}
	if _, err := ForRelay("knock.example.com"); err == nil {
		t.Fatal("a relay without a scheme should be refused")
	}
}

func writeLegacy(t *testing.T, name string, v any) string {
	t.Helper()
	p, _ := Path(name)
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoorIsPerRelay(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	a, _ := ForRelay("https://a.example")
	b, _ := ForRelay("https://b.example")
	da, err := a.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	if da.Relay != "https://a.example" || da.CodeCreatedAt.IsZero() {
		t.Fatalf("new door: %+v", da)
	}
	db, err := b.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	if db.DoorKey == da.DoorKey || db.Code == da.Code {
		t.Fatal("a second relay must get its own door key and code")
	}
	again, _, _ := a.LoadDoor()
	if again.DoorKey != da.DoorKey {
		t.Fatal("relay a's door changed")
	}

	// A door.json that names another relay is ignored.
	other := da
	other.Relay = "https://evil.example"
	if err := writeJSON(a.DoorPath(), other); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.LoadDoor(); ok || err != nil {
		t.Fatalf("a door for another relay was used: ok=%v err=%v", ok, err)
	}
	fresh, err := a.LoadOrCreateDoor()
	if err != nil || fresh.DoorKey == da.DoorKey || fresh.Relay != a.Origin {
		t.Fatalf("expected a fresh door for a: %+v %v", fresh, err)
	}
}

func TestDoorMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOCK_HOME", home)
	legacy := Door{DoorID: "bvjy", DoorKey: "key", Code: "4821KQ", CodeVersion: 3, Salt: make([]byte, 16), CodeHash: make([]byte, 32)}
	lp := writeLegacy(t, "door.json", legacy)
	s, _ := ForRelay("http://127.0.0.1:8787")

	d, ok, err := s.LoadDoor()
	if err != nil || !ok || d.DoorKey != "key" || d.CodeVersion != 3 {
		t.Fatalf("migrated door: %+v ok=%v err=%v", d, ok, err)
	}
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Fatal("the legacy door.json should be moved, not copied")
	}
	var onDisk map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "relays", "http_127.0.0.1_8787", "door.json"))
	_ = json.Unmarshal(b, &onDisk)
	if onDisk["relay"] != "http://127.0.0.1:8787" || onDisk["code_created_at"] == nil {
		t.Fatalf("the moved file should name its relay and start the clock: %s", b)
	}
	if fi, _ := os.Stat(filepath.Join(home, "relays", "http_127.0.0.1_8787")); fi.Mode().Perm() != DirMode {
		t.Fatalf("relay folder mode %v", fi.Mode().Perm())
	}

	// Idempotent; and another relay doesn't get the moved door.
	if d2, ok, _ := s.LoadDoor(); !ok || d2.DoorKey != "key" {
		t.Fatal("second load lost the door")
	}
	other, _ := ForRelay("https://other.example")
	if _, ok, _ := other.LoadDoor(); ok {
		t.Fatal("the migrated door leaked to another relay")
	}
}

func TestMigrationNeverOverwrites(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	s, _ := ForRelay("https://a.example")
	mine, err := s.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	lp := writeLegacy(t, "door.json", Door{DoorID: "zzzz", DoorKey: "old"})
	d, _, _ := s.LoadDoor()
	if d.DoorKey != mine.DoorKey {
		t.Fatal("the legacy file replaced the relay's door")
	}
	if _, err := os.Stat(lp); err != nil {
		t.Fatal("the legacy file should stay when the relay already has a door")
	}
}

func TestRecentMigrationAndIsolation(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	lp := writeLegacy(t, "recent.json", Recent{Items: []RecentItem{{DoorID: "bvjy", CodeHash: []byte{1, 2, 3}}}})
	a, _ := ForRelay("https://a.example")
	r, err := a.LoadRecent()
	if err != nil || len(r.Items) != 1 || r.Items[0].DoorID != "bvjy" {
		t.Fatalf("migrated recent: %+v %v", r, err)
	}
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Fatal("legacy recent.json should be gone")
	}
	b, _ := ForRelay("https://b.example")
	if rb, _ := b.LoadRecent(); len(rb.Items) != 0 {
		t.Fatal("another relay must start with an empty Recent list")
	}
	r.Upsert(RecentItem{DoorID: "k7f3", LastSessionAt: Now()})
	if err := a.SaveRecent(r); err != nil {
		t.Fatal(err)
	}
	if rb, _ := b.LoadRecent(); len(rb.Items) != 0 {
		t.Fatal("saving a's list touched b's")
	}
}

func TestMigrationRace(t *testing.T) {
	// Two clients migrating at once: exactly one moves the file, nobody
	// errors, and the result is the legacy door.
	t.Setenv("KNOCK_HOME", t.TempDir())
	writeLegacy(t, "door.json", Door{DoorID: "bvjy", DoorKey: "key"})
	s, _ := ForRelay("https://a.example")
	done := make(chan bool, 8)
	for range 8 {
		go func() {
			moved, err := s.migrate("door.json")
			if err != nil {
				t.Error(err)
			}
			done <- moved
		}()
	}
	n := 0
	for range 8 {
		if <-done {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d clients moved the file, want 1", n)
	}
	if d, ok, _ := s.LoadDoor(); !ok || d.DoorKey != "key" {
		t.Fatal("lost the door in the race")
	}
}

func TestCodeAge(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	d := Door{CodeCreatedAt: NewTime(now.Add(-7*24*time.Hour + time.Minute))}
	if d.CodeExpired(7, now) {
		t.Error("a code just under a week old is fine")
	}
	if !d.CodeExpired(7, now.Add(time.Minute)) {
		t.Error("a code a week old is expired")
	}
	if d.CodeExpired(0, now.Add(365*24*time.Hour)) {
		t.Error("0 means never")
	}
	if (Door{}).CodeExpired(7, now) {
		t.Error("no timestamp is not expired")
	}
	d.Rotate()
	if time.Since(d.CodeCreatedAt.Time) > time.Minute {
		t.Error("Rotate should restart the clock")
	}
}

func TestDoorWithoutCreatedAtStartsClock(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	s, _ := ForRelay("https://a.example")
	d, err := s.LoadOrCreateDoor()
	if err != nil {
		t.Fatal(err)
	}
	d.CodeCreatedAt = Time{}
	if err := writeJSON(s.DoorPath(), d); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadOrCreateDoor()
	if err != nil || got.CodeCreatedAt.IsZero() || got.Code != d.Code {
		t.Fatalf("got %+v %v", got, err)
	}
	b, _ := os.ReadFile(s.DoorPath())
	if !strings.Contains(string(b), `"code_created_at": "20`) {
		t.Fatalf("code_created_at not saved: %s", b)
	}
}

func TestSaveClaim(t *testing.T) {
	t.Setenv("KNOCK_HOME", t.TempDir())
	s, _ := ForRelay("https://a.example")
	d, _ := s.LoadOrCreateDoor()
	if err := s.SaveClaim(d.DoorID, d.DoorKey, "claim-1"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.LoadDoor()
	if got.Claim != "claim-1" || got.Code != d.Code {
		t.Fatalf("claim not saved: %+v", got)
	}
	// Another key's claim is never written onto this door.
	if err := s.SaveClaim(d.DoorID, "other-key", "claim-2"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LoadDoor(); got.Claim != "claim-1" {
		t.Fatal("claim for another key was saved")
	}
}
