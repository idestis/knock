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
		_, _ = io.WriteString(w, `{"providers":["google","github"]}`)
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
	if err != nil || strings.Join(ps, ",") != "google,github" {
		t.Fatalf("providers %v %v", ps, err)
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
	_ = Save(fresh)
	if r := Resolve(ctx, http.DefaultClient, "http://unused", now); !r.Signed || r.Tokens.AccessToken != "at" {
		t.Fatalf("fresh token not used: %+v", r)
	}

	// Close to expiry: refreshed and the rotated tokens saved.
	srv := fakeRelay(t, 200)
	_ = Save(Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Minute).Unix()})
	r := Resolve(ctx, srv.Client(), srv.URL, now)
	if !r.Signed || r.Tokens.AccessToken != "at-new" {
		t.Fatalf("not refreshed: %+v", r)
	}
	if saved, _, _ := Load(); saved.RefreshToken != "rt-new" {
		t.Fatalf("rotated refresh token not saved: %+v", saved)
	}

	// Refresh rejected with 401: token dropped, anonymous with a warning.
	bad := fakeRelay(t, 401)
	_ = Save(Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Minute).Unix()})
	r = Resolve(ctx, bad.Client(), bad.URL, now)
	if r.Signed || !strings.Contains(r.Warning, "anonymous") {
		t.Fatalf("want anonymous with warning: %+v", r)
	}
	if _, ok, _ := Load(); ok {
		t.Fatal("rejected token should be deleted")
	}

	// Network failure with a still-valid token: keep using it.
	_ = Save(Tokens{AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(time.Minute).Unix()})
	r = Resolve(ctx, http.DefaultClient, "http://127.0.0.1:1", now)
	if !r.Signed || r.Tokens.AccessToken != "at" {
		t.Fatalf("valid token dropped on network error: %+v", r)
	}
}
