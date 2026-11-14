package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/idestis/knock/protocol"
)

// fakeProviders stands in for Google and GitHub. The provider "code" picks
// which user comes back, so one server covers every case.
type fakeUser struct {
	google map[string]any // userinfo body
	gh     map[string]any // /user body
	emails []map[string]any
	org    int    // membership status code
	state  string // membership state
}

const providerSecretToken = "provider-access-token-must-not-leak"

func fakeProviders(t *testing.T, users map[string]fakeUser) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	byToken := func(r *http.Request) (fakeUser, bool) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "+providerSecretToken+":")
		u, ok := users[tok]
		return u, ok
	}
	token := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "shh" || r.Form.Get("grant_type") != "authorization_code" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_client","access_token_echo":"`+providerSecretToken+`"}`)
			return
		}
		if _, ok := users[r.Form.Get("code")]; !ok {
			_, _ = io.WriteString(w, `{"error":"bad_verification_code"}`) // GitHub style: 200 + error
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": providerSecretToken + ":" + r.Form.Get("code")})
	}
	mux.HandleFunc("POST /google/token", token)
	mux.HandleFunc("POST /github/token", token)
	mux.HandleFunc("GET /google/userinfo", func(w http.ResponseWriter, r *http.Request) {
		u, ok := byToken(r)
		if !ok {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(u.google)
	})
	mux.HandleFunc("GET /gh/user", func(w http.ResponseWriter, r *http.Request) {
		u, _ := byToken(r)
		_ = json.NewEncoder(w).Encode(u.gh)
	})
	mux.HandleFunc("GET /gh/user/emails", func(w http.ResponseWriter, r *http.Request) {
		u, _ := byToken(r)
		_ = json.NewEncoder(w).Encode(u.emails)
	})
	mux.HandleFunc("GET /gh/user/memberships/orgs/{org}", func(w http.ResponseWriter, r *http.Request) {
		u, _ := byToken(r)
		if r.PathValue("org") != "acme" || u.org != 200 {
			w.WriteHeader(max(u.org, 404))
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"state": u.state})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var testUsers = map[string]fakeUser{
	"g-ok":       {google: map[string]any{"sub": "111", "name": "Maya Patel", "email": "maya@acme.dev", "email_verified": true, "hd": "acme.dev", "picture": "https://x/y.png"}},
	"g-gmail":    {google: map[string]any{"sub": "222", "name": "Sam", "email": "sam@gmail.com", "email_verified": true}},
	"g-unverif":  {google: map[string]any{"sub": "333", "name": "Eve", "email": "eve@acme.dev", "email_verified": false, "hd": "acme.dev"}},
	"g-control":  {google: map[string]any{"sub": "444", "name": "Bad\x1b[2JName‮", "email": "b@acme.dev", "email_verified": true, "hd": "acme.dev", "picture": "javascript:alert(1)"}},
	"gh-member":  {gh: map[string]any{"id": 7, "login": "maya", "name": ""}, emails: []map[string]any{{"email": "m@acme.dev", "primary": true, "verified": true}}, org: 200, state: "active"},
	"gh-pending": {gh: map[string]any{"id": 8, "login": "inv", "name": "Invited"}, org: 200, state: "pending"},
	"gh-outside": {gh: map[string]any{"id": 9, "login": "out", "name": "Outsider"}, emails: []map[string]any{{"email": "o@x.dev", "primary": true, "verified": false}}, org: 404},
	"gh-blocked": {gh: map[string]any{"id": 10, "login": "blk", "name": "Blocked"}, org: 403},
}

type harness struct {
	t     *testing.T
	svc   *Service
	relay *httptest.Server
	logs  *bytes.Buffer
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	fp := fakeProviders(t, testUsers)
	ep := endpoints{
		googleAuth: fp.URL + "/google/auth", googleToken: fp.URL + "/google/token", googleUserinfo: fp.URL + "/google/userinfo",
		githubAuth: fp.URL + "/github/auth", githubToken: fp.URL + "/github/token", githubAPI: fp.URL + "/gh",
	}
	if cfg.GoogleID == "" && cfg.GitHubID == "" {
		cfg.GoogleID, cfg.GitHubID = "gid", "hid"
	}
	cfg.GoogleSecret, cfg.GitHubSecret = "shh", "shh"
	svc := newService(cfg, NewSigner([]byte("0123456789abcdef0123456789abcdef")), ep)
	mux := http.NewServeMux()
	svc.Register(mux)
	relay := httptest.NewServer(mux)
	t.Cleanup(relay.Close)
	svc.cfg.PublicURL = relay.URL

	logs := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &harness{t: t, svc: svc, relay: relay, logs: logs}
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func pkce() (string, string) {
	v := RandomString(32)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

// start runs /start and returns the relay state sent to the provider.
func (h *harness) start(provider, challenge string) (relayState string, status int, providerURL *url.URL) {
	h.t.Helper()
	v := url.Values{"redirect_uri": {"http://127.0.0.1:5555/callback"}, "state": {"client-state"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	resp, err := noFollow.Get(h.relay.URL + "/v1/auth/" + provider + "/start?" + v.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", resp.StatusCode, nil
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	return u.Query().Get("state"), resp.StatusCode, u
}

// callback plays the provider redirecting back; returns the client redirect query.
func (h *harness) callback(provider, state, code string) url.Values {
	h.t.Helper()
	resp, err := noFollow.Get(h.relay.URL + "/v1/auth/" + provider + "/callback?" + url.Values{"state": {state}, "code": {code}}.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return url.Values{"status": {resp.Status}}
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(u.String(), "http://127.0.0.1:5555/callback?") {
		h.t.Fatalf("redirected to %s", u)
	}
	return u.Query()
}

func (h *harness) post(path string, body any) (int, TokenResponse) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(h.relay.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var tr TokenResponse
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	return resp.StatusCode, tr
}

// signIn runs the whole flow; returns the client redirect and, on success, tokens.
func (h *harness) signIn(provider, user string) (url.Values, TokenResponse) {
	h.t.Helper()
	verifier, challenge := pkce()
	state, status, _ := h.start(provider, challenge)
	if status != http.StatusFound {
		h.t.Fatalf("start: %d", status)
	}
	back := h.callback(provider, state, user)
	if back.Get("state") != "client-state" {
		h.t.Fatalf("client state not returned: %v", back)
	}
	if back.Get("code") == "" {
		return back, TokenResponse{}
	}
	code, tr := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": verifier})
	if code != 200 {
		h.t.Fatalf("token: %d", code)
	}
	return back, tr
}

func (h *harness) assertNoSecretsLogged() {
	h.t.Helper()
	if strings.Contains(h.logs.String(), providerSecretToken) || strings.Contains(h.logs.String(), "shh") {
		h.t.Fatalf("a token or secret reached the log:\n%s", h.logs)
	}
}

func TestSignInRestrictions(t *testing.T) {
	cfg := Config{GoogleID: "gid", GitHubID: "hid", GoogleDomain: "acme.dev", GitHubOrg: "acme"}
	for _, tc := range []struct {
		provider, user string
		wantID         string // empty: refused
		wantErr        string // substring of error_description
		check          func(t *testing.T, id protocol.Identity)
	}{
		{"google", "g-ok", "google:111", "", func(t *testing.T, id protocol.Identity) {
			if id.Email != "maya@acme.dev" || id.Name != "Maya Patel" || id.Provider != "google" || !id.Verified || id.Login != "" {
				t.Fatalf("identity %+v", id)
			}
		}},
		{"google", "g-gmail", "", "Only acme.dev Google accounts", nil},
		{"google", "g-unverif", "", "verified email", nil},
		{"google", "g-control", "google:444", "", func(t *testing.T, id protocol.Identity) {
			if id.Name != "Bad[2JName" || id.Avatar != "" {
				t.Fatalf("not cleaned: %q avatar %q", id.Name, id.Avatar)
			}
		}},
		{"google", "nobody", "", "Sign-in didn't work", nil},
		{"github", "gh-member", "github:7", "", func(t *testing.T, id protocol.Identity) {
			if id.Login != "maya" || id.Name != "maya" || id.Email != "m@acme.dev" || id.Provider != "github" {
				t.Fatalf("identity %+v", id)
			}
		}},
		{"github", "gh-pending", "", "Only members of the acme GitHub organization", nil},
		{"github", "gh-outside", "", "Only members of the acme GitHub organization", nil},
		{"github", "gh-blocked", "", "org owner may need to approve", nil},
	} {
		t.Run(tc.provider+"/"+tc.user, func(t *testing.T) {
			h := newHarness(t, cfg)
			back, tr := h.signIn(tc.provider, tc.user)
			if tc.wantID == "" {
				if back.Get("code") != "" || !strings.Contains(back.Get("error_description"), tc.wantErr) {
					t.Fatalf("want refusal %q, got %v", tc.wantErr, back)
				}
			} else {
				if tr.User.ID != tc.wantID {
					t.Fatalf("want %s, got %+v (%v)", tc.wantID, tr.User, back)
				}
				c, err := h.svc.signer.Verify(tr.AccessToken, KindAccess)
				if err != nil || c.Identity() != tr.User {
					t.Fatalf("access token %+v %v", c, err)
				}
				tc.check(t, tr.User)
			}
			h.assertNoSecretsLogged()
		})
	}
}

func TestStartValidation(t *testing.T) {
	h := newHarness(t, Config{})
	_, challenge := pkce()
	good := url.Values{"redirect_uri": {"knock://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}}
	for _, tc := range []struct {
		name   string
		path   string
		mutate func(v url.Values)
		want   int
	}{
		{"ok, method defaults to S256", "google", func(url.Values) {}, 302},
		{"unknown provider", "gitlab", func(url.Values) {}, 404},
		{"https redirect", "google", func(v url.Values) { v.Set("redirect_uri", "https://evil.example/cb") }, 400},
		{"remote http redirect", "google", func(v url.Values) { v.Set("redirect_uri", "http://evil.example/cb") }, 400},
		{"cli loopback", "google", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:49152/callback") }, 302},
		{"localhost loopback", "google", func(v url.Values) { v.Set("redirect_uri", "http://localhost:49152/callback") }, 302},
		{"ipv6 loopback", "google", func(v url.Values) { v.Set("redirect_uri", "http://[::1]:49152/callback") }, 302},
		{"other knock path", "google", func(v url.Values) { v.Set("redirect_uri", "knock://evil/callback") }, 400},
		{"knock with query", "google", func(v url.Values) { v.Set("redirect_uri", "knock://auth/callback?x=1") }, 400},
		{"loopback other path", "google", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:5555/steal") }, 400},
		{"loopback with query", "google", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:5555/callback?x=1") }, 400},
		{"loopback with fragment", "google", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:5555/callback#x") }, 400},
		{"loopback with userinfo", "google", func(v url.Values) { v.Set("redirect_uri", "http://u@127.0.0.1:5555/callback") }, 400},
		{"loopback bad port", "google", func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:99999/callback") }, 400},
		{"huge redirect", "google", func(v url.Values) {
			v.Set("redirect_uri", "http://127.0.0.1:5555/callback"+strings.Repeat("/", 300))
		}, 400},
		{"huge knock redirect", "google", func(v url.Values) {
			v.Set("redirect_uri", "knock://auth/callback/"+strings.Repeat("A", 64<<10))
		}, 400},
		{"no state", "google", func(v url.Values) { v.Del("state") }, 400},
		{"short challenge", "google", func(v url.Values) { v.Set("code_challenge", "abc") }, 400},
		{"plain method", "google", func(v url.Values) { v.Set("code_challenge_method", "plain") }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := url.Values{}
			for k, vs := range good {
				v[k] = append([]string(nil), vs...)
			}
			tc.mutate(v)
			resp, err := noFollow.Get(h.relay.URL + "/v1/auth/" + tc.path + "/start?" + v.Encode())
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	// The provider gets our state, not the client's, plus the hd hint.
	h2 := newHarness(t, Config{GoogleID: "gid", GoogleDomain: "acme.dev"})
	state, _, u := h2.start("google", challenge)
	if state == "" || state == "client-state" || u.Query().Get("hd") != "acme.dev" ||
		u.Query().Get("redirect_uri") != h2.relay.URL+"/v1/auth/google/callback" {
		t.Fatalf("provider redirect %s", u)
	}
}

func TestCallbackAndGrant(t *testing.T) {
	h := newHarness(t, Config{})
	verifier, challenge := pkce()

	// Unknown state.
	if got := h.callback("google", "forged", "g-ok"); got.Get("status") == "" {
		t.Fatalf("forged state accepted: %v", got)
	}
	// State from one provider can't finish on another.
	state, _, _ := h.start("google", challenge)
	if got := h.callback("github", state, "gh-member"); got.Get("status") == "" {
		t.Fatalf("cross-provider state accepted: %v", got)
	}
	// ...and it was spent.
	if got := h.callback("google", state, "g-ok"); got.Get("status") == "" {
		t.Fatalf("state reused: %v", got)
	}

	// User says no at the provider.
	state, _, _ = h.start("google", challenge)
	resp, _ := noFollow.Get(h.relay.URL + "/v1/auth/google/callback?state=" + url.QueryEscape(state) + "&error=access_denied")
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "client-state" {
		t.Fatalf("denied redirect %s", loc)
	}

	// Wrong verifier burns the grant.
	state, _, _ = h.start("google", challenge)
	back := h.callback("google", state, "g-ok")
	if code, _ := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": "wrong"}); code != 400 {
		t.Fatalf("wrong verifier: %d", code)
	}
	if code, _ := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": verifier}); code != 400 {
		t.Fatalf("grant survived a failed try: %d", code)
	}

	// Right verifier works once.
	state, _, _ = h.start("google", challenge)
	back = h.callback("google", state, "g-ok")
	if code, tr := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": verifier}); code != 200 || tr.RefreshToken == "" {
		t.Fatalf("token: %d", code)
	}
	if code, _ := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": verifier}); code != 400 {
		t.Fatalf("grant reused: %d", code)
	}

	// Grants and pending sign-ins expire.
	state, _, _ = h.start("google", challenge)
	back = h.callback("google", state, "g-ok")
	pendingState, _, _ := h.start("google", challenge)
	h.svc.expire(time.Now().Add(pendingTTL + time.Second))
	if code, _ := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": verifier}); code != 400 {
		t.Fatalf("expired grant: %d", code)
	}
	if got := h.callback("google", pendingState, "g-ok"); got.Get("status") == "" {
		t.Fatalf("expired state accepted: %v", got)
	}
	h.assertNoSecretsLogged()
}

func TestRefresh(t *testing.T) {
	cfg := Config{GoogleID: "gid", GitHubID: "hid", GitHubOrg: "acme"}
	h := newHarness(t, cfg)
	_, tr := h.signIn("github", "gh-member")

	code, fresh := h.post("/v1/auth/refresh", map[string]string{"refresh_token": tr.RefreshToken})
	if code != 200 || fresh.User != tr.User || fresh.AccessToken == tr.AccessToken || fresh.RefreshUntil < tr.RefreshUntil {
		t.Fatalf("refresh %d %+v", code, fresh)
	}
	if code, _ := h.post("/v1/auth/refresh", map[string]string{"refresh_token": tr.AccessToken}); code != 401 {
		t.Fatalf("access token used as refresh: %d", code)
	}
	if code, _ := h.post("/v1/auth/refresh", map[string]string{"refresh_token": tr.RefreshToken + "x"}); code != 401 {
		t.Fatalf("tampered refresh: %d", code)
	}

	// The operator changes the org: old sign-ins stop refreshing.
	h.svc.cfg.GitHubOrg = "other"
	if code, _ := h.post("/v1/auth/refresh", map[string]string{"refresh_token": fresh.RefreshToken}); code != 401 {
		t.Fatalf("refresh survived an org change: %d", code)
	}
	h.svc.cfg.GitHubOrg = "acme"

	// The operator removes GitHub sign-in.
	delete(h.svc.providers, "github")
	if code, _ := h.post("/v1/auth/refresh", map[string]string{"refresh_token": fresh.RefreshToken}); code != 401 {
		t.Fatalf("refresh survived provider removal: %d", code)
	}
}

func TestProvidersEndpoint(t *testing.T) {
	h := newHarness(t, Config{GitHubID: "hid", SignInRequired: true})
	resp, err := http.Get(h.relay.URL + "/v1/auth/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p protocol.AuthProviders
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if len(p.Providers) != 1 || p.Providers[0] != "github" || !p.SignInRequired {
		t.Fatalf("providers %+v", p)
	}
}

func TestTokens(t *testing.T) {
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	id := protocol.Identity{ID: "github:1", Name: "Maya", Login: "maya", Provider: "github", Verified: true}
	tok, _ := s.Issue(KindAccess, id, time.Minute)
	if c, err := s.Verify(tok, KindAccess); err != nil || c.Identity() != id {
		t.Fatalf("round trip %+v %v", c, err)
	}
	expired, _ := s.Issue(KindAccess, id, -time.Second)
	other := NewSigner([]byte("another-secret-another-secret-xx"))
	for name, tc := range map[string]struct{ tok, kind string }{
		"wrong kind":   {tok, KindRefresh},
		"expired":      {expired, KindAccess},
		"other secret": {func() string { t, _ := other.Issue(KindAccess, id, time.Minute); return t }(), KindAccess},
		"no dot":       {"abc", KindAccess},
		"empty":        {"", KindAccess},
	} {
		if _, err := s.Verify(tc.tok, tc.kind); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func (h *harness) startFrom(forwardedFor string) int {
	h.t.Helper()
	_, challenge := pkce()
	v := url.Values{"redirect_uri": {"knock://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}}
	req, _ := http.NewRequest(http.MethodGet, h.relay.URL+"/v1/auth/google/start?"+v.Encode(), nil)
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// One client can't fill the global pending table and lock everyone else
// out of sign-in.
func TestStartLimitedPerClient(t *testing.T) {
	h := newHarness(t, Config{})
	for i := range startsPerIP {
		if got := h.startFrom("203.0.113.7"); got != http.StatusFound {
			t.Fatalf("start %d: %d", i, got)
		}
	}
	if got := h.startFrom("203.0.113.7, 10.0.0.1"); got != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", got)
	}
	h.svc.mu.Lock()
	n := len(h.svc.pending)
	h.svc.mu.Unlock()
	if n != startsPerIP {
		t.Fatalf("pending %d, want %d", n, startsPerIP)
	}
	// Someone else still gets in, and so does the direct peer.
	if got := h.startFrom("198.51.100.2"); got != http.StatusFound {
		t.Fatalf("other client: %d", got)
	}
	if got := h.startFrom(""); got != http.StatusFound {
		t.Fatalf("direct client: %d", got)
	}
}

func TestStartUsesConfiguredClientIP(t *testing.T) {
	h := newHarness(t, Config{ClientIP: func(*http.Request) string { return "same" }})
	for range startsPerIP {
		h.startFrom(RandomString(8))
	}
	// A spoofed X-Forwarded-For doesn't help once the relay picks the address.
	if got := h.startFrom("192.0.2.99"); got != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", got)
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	if got := ClientIP(r); got != "192.0.2.1" {
		t.Fatalf("peer: %q", got)
	}
	r.Header.Set("X-Forwarded-For", " 203.0.113.7 , 10.0.0.1")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("forwarded: %q", got)
	}
}

// A wrong verifier is still refused with the constant-time compare.
func TestTokenWrongVerifier(t *testing.T) {
	h := newHarness(t, Config{})
	_, challenge := pkce()
	state, _, _ := h.start("google", challenge)
	back := h.callback("google", state, "g-ok")
	if code, _ := h.post("/v1/auth/token", map[string]string{"code": back.Get("code"), "code_verifier": "wrong"}); code != 400 {
		t.Fatalf("wrong verifier: %d", code)
	}
}
