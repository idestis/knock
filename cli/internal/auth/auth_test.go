package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idestis/knock/cli/internal/state"
	"github.com/idestis/knock/protocol"
)

// fakeRelay implements the relay's auth endpoints well enough for the client.
func fakeRelay(t *testing.T, refreshStatus int) *httptest.Server {
	t.Helper()
	var challenge string
	mux := http.NewServeMux()
	issue := func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(Tokens{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			RefreshUntil: time.Now().Add(90 * 24 * time.Hour).Unix(), User: protocol.Identity{ID: "github:1", Name: "Maya Patel", Provider: "github", Verified: true}})
	}
	mux.HandleFunc("GET /v1/auth/providers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"providers":["google","github"],"sign_in_required":true}`)
	})
	mux.HandleFunc("GET /v1/auth/{p}/start", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" || len(challenge) < 43 {
			http.Error(w, "bad", 400)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=grant123&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code, Code_verifier string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		sum := sha256.Sum256([]byte(in.Code_verifier))
		if in.Code != "grant123" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			w.WriteHeader(400)
			return
		}
		issue(w)
	})
	mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		if refreshStatus != 200 {
			w.WriteHeader(refreshStatus)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		issue(w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginPKCE(t *testing.T) {
	keyring.MockInit()
	srv := fakeRelay(t, 200)
	ps, err := Providers(context.Background(), srv.Client(), srv.URL)
	if err != nil || strings.Join(ps.Providers, ",") != "google,github" || !ps.SignInRequired {
		t.Fatalf("providers %+v %v", ps, err)
	}
	tok, err := Login(context.Background(), LoginOptions{Relay: srv.URL, Provider: "github", Timeout: 5 * time.Second,
		Open: func(u string) error {
			go func() {
				resp, err := http.Get(u) // follows the redirect to our loopback listener
				if err == nil {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if !strings.Contains(string(b), "You can close this tab") {
						t.Errorf("callback page: %s", b)
					}
				}
			}()
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-new" || tok.User.Name != "Maya Patel" {
		t.Fatalf("tokens %+v", tok)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	keyring.MockInit()
	if r := Resolve(ctx, http.DefaultClient, "http://unused", now); r.Signed || r.Warning != "" {
		t.Fatalf("no entry should be quietly anonymous: %+v", r)
	}

	// Fresh token: used as-is, no network.
	fresh := Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Hour).Unix()}
	_ = Save(origin("http://unused"), fresh)
	if r := Resolve(ctx, http.DefaultClient, "http://unused", now); !r.Signed || r.Tokens.AccessToken != "at" {
		t.Fatalf("fresh token not used: %+v", r)
	}

	// Close to expiry: refreshed and the rotated tokens saved.
	srv := fakeRelay(t, 200)
	_ = Save(origin(srv.URL), Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Minute).Unix()})
	r := Resolve(ctx, srv.Client(), srv.URL, now)
	if !r.Signed || r.Tokens.AccessToken != "at-new" {
		t.Fatalf("not refreshed: %+v", r)
	}
	if saved, _, _ := Load(origin(srv.URL)); saved.RefreshToken != "rt-new" {
		t.Fatalf("rotated refresh token not saved: %+v", saved)
	}

	// Refresh rejected with 401: token dropped, anonymous with a warning.
	bad := fakeRelay(t, 401)
	_ = Save(origin(bad.URL), Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Minute).Unix()})
	r = Resolve(ctx, bad.Client(), bad.URL, now)
	if r.Signed || !strings.Contains(r.Warning, "anonymous") {
		t.Fatalf("want anonymous with warning: %+v", r)
	}
	if _, ok, _ := Load(origin(bad.URL)); ok {
		t.Fatal("rejected token should be deleted")
	}

	// Network failure with a still-valid token: keep using it.
	_ = Save(origin("http://127.0.0.1:1"), Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Minute).Unix()})
	r = Resolve(ctx, http.DefaultClient, "http://127.0.0.1:1", now)
	if !r.Signed || r.Tokens.AccessToken != "at" {
		t.Fatalf("valid token dropped on network error: %+v", r)
	}

	// Past the refresh deadline: dropped without asking the relay.
	_ = Save(origin("http://127.0.0.1:1"), Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix(), RefreshUntil: now.Add(-time.Minute).Unix()})
	r = Resolve(ctx, http.DefaultClient, "http://127.0.0.1:1", now)
	if r.Signed || !strings.Contains(r.Warning, "expired") {
		t.Fatalf("want expired: %+v", r)
	}
	if _, ok, _ := Load(origin("http://127.0.0.1:1")); ok {
		t.Fatal("expired sign-in should be deleted")
	}
}

func TestForceRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	keyring.MockInit()

	// The relay refused a token that still looks valid locally (clock skew,
	// rotated secret): refresh even though it isn't due.
	srv := fakeRelay(t, 200)
	stale := Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Hour).Unix()}
	_ = Save(origin(srv.URL), stale)
	if r := ForceRefresh(ctx, srv.Client(), srv.URL, stale, now); !r.Signed || r.Tokens.AccessToken != "at-new" {
		t.Fatalf("not refreshed: %+v", r)
	}

	// The relay refuses the refresh too: signed out, so the next dial is anonymous.
	bad := fakeRelay(t, 401)
	_ = Save(origin(bad.URL), stale)
	if r := ForceRefresh(ctx, bad.Client(), bad.URL, stale, now); r.Signed || r.Warning == "" {
		t.Fatalf("want signed out: %+v", r)
	}
	if _, ok, _ := Load(origin(bad.URL)); ok {
		t.Fatal("rejected sign-in should be deleted")
	}

	// Relay unreachable: the forced refresh can't keep a token the relay refused.
	_ = Save(origin("http://127.0.0.1:1"), stale)
	if r := ForceRefresh(ctx, http.DefaultClient, "http://127.0.0.1:1", stale, now); r.Signed {
		t.Fatalf("kept a refused token: %+v", r)
	}
}

func origin(u string) string {
	o, err := state.Origin(u)
	if err != nil {
		panic(err)
	}
	return o
}

// A sign-in belongs to the relay that issued it: pointing knock at another
// relay is anonymous there, and its tokens are never sent.
func TestSignInIsPerRelay(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	keyring.MockInit()
	a := fakeRelay(t, 200)
	_ = Save(origin(a.URL), Tokens{AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: now.Add(time.Hour).Unix()})

	var sawRefresh bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawRefresh = true }))
	t.Cleanup(other.Close)
	r := Resolve(ctx, other.Client(), other.URL, now)
	if r.Signed || r.Tokens.AccessToken != "" || sawRefresh {
		t.Fatalf("relay a's sign-in reached another relay: %+v", r)
	}
	if r := Resolve(ctx, a.Client(), a.URL+"/", now); !r.Signed || r.Tokens.AccessToken != "at-a" {
		t.Fatalf("the same relay spelt with a slash lost its sign-in: %+v", r)
	}
	if got := Account("https://knock.example.com"); got != "session:https://knock.example.com" {
		t.Fatalf("account = %q", got)
	}
}

func TestLegacySignInMoves(t *testing.T) {
	keyring.MockInit()
	legacy := `{"access_token":"at-old","refresh_token":"rt-old","expires_at":1}`
	_ = keyring.Set(KeychainService, LegacyAccount, legacy)
	o := origin("https://knock.example.com")
	got, ok, err := Load(o)
	if err != nil || !ok || got.AccessToken != "at-old" {
		t.Fatalf("legacy sign-in not moved: %+v %v %v", got, ok, err)
	}
	if _, err := keyring.Get(KeychainService, LegacyAccount); err == nil {
		t.Fatal("the legacy account should be gone after the move")
	}
	if s, err := keyring.Get(KeychainService, Account(o)); err != nil || s != legacy {
		t.Fatalf("not stored under the relay's account: %q %v", s, err)
	}
	// Idempotent, and a second relay doesn't inherit it.
	if _, ok, _ := Load(o); !ok {
		t.Fatal("second load lost the sign-in")
	}
	if _, ok, _ := Load(origin("https://other.example")); ok {
		t.Fatal("the moved sign-in leaked to another relay")
	}

	// Knock.app already moved and refreshed it: its copy wins.
	keyring.MockInit()
	_ = keyring.Set(KeychainService, LegacyAccount, legacy)
	_ = keyring.Set(KeychainService, Account(o), `{"access_token":"at-app","refresh_token":"rt-app"}`)
	if got, _, _ := Load(o); got.AccessToken != "at-app" {
		t.Fatalf("the app's newer sign-in was replaced: %+v", got)
	}

	// A leftover legacy sign-in next to the relay's own is dropped, so it
	// never moves to the next relay the user points at.
	keyring.MockInit()
	_ = keyring.Set(KeychainService, Account(o), `{"access_token":"at-mine","refresh_token":"rt-mine"}`)
	_ = keyring.Set(KeychainService, LegacyAccount, legacy)
	if got, _, _ := Load(o); got.AccessToken != "at-mine" {
		t.Fatalf("the relay's own sign-in was replaced: %+v", got)
	}
	if _, err := keyring.Get(KeychainService, LegacyAccount); err == nil {
		t.Fatal("the leftover legacy sign-in should be deleted")
	}
	if _, ok, _ := Load(origin("https://other.example")); ok {
		t.Fatal("the leftover sign-in moved to another relay")
	}
}

func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		id   protocol.Identity
		want string
	}{
		{protocol.Identity{Email: "m@acme.dev", Login: "maya"}, "m@acme.dev"},
		{protocol.Identity{Login: "maya"}, "@maya"},
		{protocol.Identity{Name: "Guest"}, ""},
	} {
		if got := Describe(tc.id); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
