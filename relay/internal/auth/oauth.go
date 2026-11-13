package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idestis/knock/protocol"
)

// Provider is one OAuth sign-in option.
type Provider struct {
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Scopes       string
	// Fetch turns a provider access token into an identity.
	Fetch func(ctx context.Context, hc *http.Client, accessToken string) (protocol.Identity, error)
}

// Config for the OAuth service.
type Config struct {
	PublicURL    string // https://relay.example.com, used for provider callbacks
	GoogleID     string
	GoogleSecret string
	GoogleDomain string // optional Workspace hd restriction
	GitHubID     string
	GitHubSecret string
	GitHubOrg    string // optional: require membership (needs read:org)
}

// Service runs the relay-mediated OAuth flow:
//
//  1. Client opens /v1/auth/{provider}/start?redirect_uri=knock://auth&state=S&code_challenge=C
//  2. Relay redirects to the provider; the provider calls back the relay.
//  3. Relay redirects to redirect_uri?code=G&state=S with a one-time grant.
//  4. Client POSTs /v1/auth/token {code: G, code_verifier} and gets an access
//     token (1 h) plus a refresh token (90 d, sliding) to keep in the Keychain.
//
// PKCE between the client and the relay means a stolen grant is useless.
// Provider secrets stay on the relay.
type Service struct {
	cfg       Config
	signer    *Signer
	providers map[string]*Provider
	hc        *http.Client

	mu      sync.Mutex
	pending map[string]pendingAuth // keyed by relay state
	grants  map[string]grant       // keyed by one-time code
}

type pendingAuth struct {
	provider, redirect, clientState, challenge string
	expires                                    time.Time
}

type grant struct {
	id        protocol.Identity
	challenge string
	expires   time.Time
}

func NewService(cfg Config, signer *Signer) *Service {
	s := &Service{cfg: cfg, signer: signer, providers: map[string]*Provider{},
		hc: &http.Client{Timeout: 15 * time.Second}, pending: map[string]pendingAuth{}, grants: map[string]grant{}}
	if cfg.GoogleID != "" {
		s.providers["google"] = googleProvider(cfg)
	}
	if cfg.GitHubID != "" {
		s.providers["github"] = githubProvider(cfg)
	}
	go s.sweep()
	return s
}

func (s *Service) Providers() []string {
	out := []string{}
	for _, n := range []string{"google", "github"} {
		if _, ok := s.providers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (s *Service) Signer() *Signer { return s.signer }

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/auth/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"providers": s.Providers()})
	})
	mux.HandleFunc("GET /v1/auth/{provider}/start", s.start)
	mux.HandleFunc("GET /v1/auth/{provider}/callback", s.callback)
	mux.HandleFunc("POST /v1/auth/token", s.token)
	mux.HandleFunc("POST /v1/auth/refresh", s.refresh)
}

func validRedirect(u string) bool {
	if strings.HasPrefix(u, "knock://") {
		return true
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" {
		return false
	}
	h := p.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (s *Service) start(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("provider")]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	redirect, clientState, challenge := q.Get("redirect_uri"), q.Get("state"), q.Get("code_challenge")
	if !validRedirect(redirect) || clientState == "" || len(challenge) < 43 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	state := RandomString(24)
	s.mu.Lock()
	s.pending[state] = pendingAuth{provider: p.Name, redirect: redirect, clientState: clientState, challenge: challenge,
		expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()

	v := url.Values{}
	v.Set("client_id", p.ClientID)
	v.Set("redirect_uri", s.callbackURL(p.Name))
	v.Set("response_type", "code")
	v.Set("scope", p.Scopes)
	v.Set("state", state)
	if p.Name == "google" {
		v.Set("prompt", "select_account")
		if s.cfg.GoogleDomain != "" {
			v.Set("hd", s.cfg.GoogleDomain)
		}
	}
	http.Redirect(w, r, p.AuthURL+"?"+v.Encode(), http.StatusFound)
}

func (s *Service) callbackURL(provider string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/v1/auth/" + provider + "/callback"
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	pa, ok := s.pending[q.Get("state")]
	delete(s.pending, q.Get("state"))
	s.mu.Unlock()
	if !ok || time.Now().After(pa.expires) || pa.provider != r.PathValue("provider") {
		http.Error(w, "sign-in expired, try again", http.StatusBadRequest)
		return
	}
	back := func(params url.Values) {
		params.Set("state", pa.clientState)
		sep := "?"
		if strings.Contains(pa.redirect, "?") {
			sep = "&"
		}
		http.Redirect(w, r, pa.redirect+sep+params.Encode(), http.StatusFound)
	}
	if e := q.Get("error"); e != "" {
		back(url.Values{"error": {e}})
		return
	}
	p := s.providers[pa.provider]
	id, err := s.exchange(r.Context(), p, q.Get("code"))
	if err != nil {
		slog.Warn("sign-in failed", "provider", p.Name, "err", err)
		back(url.Values{"error": {"sign_in_failed"}, "error_description": {err.Error()}})
		return
	}
	code := RandomString(24)
	s.mu.Lock()
	s.grants[code] = grant{id: id, challenge: pa.challenge, expires: time.Now().Add(2 * time.Minute)}
	s.mu.Unlock()
	slog.Info("signed in", "provider", p.Name, "user", id.ID)
	back(url.Values{"code": {code}})
}

func (s *Service) exchange(ctx context.Context, p *Provider, code string) (protocol.Identity, error) {
	v := url.Values{}
	v.Set("client_id", p.ClientID)
	v.Set("client_secret", p.ClientSecret)
	v.Set("code", code)
	v.Set("grant_type", "authorization_code")
	v.Set("redirect_uri", s.callbackURL(p.Name))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(s.hc, req, &tok); err != nil {
		return protocol.Identity{}, err
	}
	if tok.AccessToken == "" {
		return protocol.Identity{}, fmt.Errorf("token exchange: %s", tok.Error)
	}
	return p.Fetch(ctx, s.hc, tok.AccessToken)
}

// TokenResponse is returned by /v1/auth/token and /v1/auth/refresh.
type TokenResponse struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token"`
	ExpiresAt    int64             `json:"expires_at"`
	RefreshUntil int64             `json:"refresh_expires_at"`
	User         protocol.Identity `json:"user"`
}

func (s *Service) issue(id protocol.Identity) TokenResponse {
	at, aexp := s.signer.Issue(KindAccess, id, AccessTTL)
	rt, rexp := s.signer.Issue(KindRefresh, id, RefreshTTL)
	return TokenResponse{AccessToken: at, RefreshToken: rt, ExpiresAt: aexp.Unix(), RefreshUntil: rexp.Unix(), User: id}
}

func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Verifier string `json:"code_verifier"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad_request"})
		return
	}
	s.mu.Lock()
	g, ok := s.grants[in.Code]
	delete(s.grants, in.Code)
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(in.Verifier))
	if !ok || time.Now().After(g.expires) || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(w, 200, s.issue(g.id))
}

func (s *Service) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
		writeJSON(w, 400, map[string]string{"error": "bad_request"})
		return
	}
	c, err := s.signer.Verify(in.RefreshToken, KindRefresh)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(w, 200, s.issue(c.Identity()))
}

func (s *Service) sweep() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		s.mu.Lock()
		for k, v := range s.pending {
			if now.After(v.expires) {
				delete(s.pending, k)
			}
		}
		for k, v := range s.grants {
			if now.After(v.expires) {
				delete(s.grants, k)
			}
		}
		s.mu.Unlock()
	}
}

func googleProvider(cfg Config) *Provider {
	return &Provider{
		Name: "google", ClientID: cfg.GoogleID, ClientSecret: cfg.GoogleSecret,
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		Scopes: "openid email profile",
		Fetch: func(ctx context.Context, hc *http.Client, at string) (protocol.Identity, error) {
			var u struct {
				Sub, Name, Email, Picture, HD string
				EmailVerified                 bool `json:"email_verified"`
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
			req.Header.Set("Authorization", "Bearer "+at)
			if err := doJSON(hc, req, &u); err != nil {
				return protocol.Identity{}, err
			}
			if u.Sub == "" || !u.EmailVerified {
				return protocol.Identity{}, fmt.Errorf("google account has no verified email")
			}
			if cfg.GoogleDomain != "" && !strings.EqualFold(u.HD, cfg.GoogleDomain) {
				return protocol.Identity{}, fmt.Errorf("only %s accounts can sign in", cfg.GoogleDomain)
			}
			if u.Name == "" {
				u.Name = u.Email
			}
			return protocol.Identity{ID: "google:" + u.Sub, Name: u.Name, Email: u.Email, Provider: "google", Verified: true, Avatar: u.Picture}, nil
		},
	}
}

func githubProvider(cfg Config) *Provider {
	scopes := "read:user user:email"
	if cfg.GitHubOrg != "" {
		scopes += " read:org"
	}
	return &Provider{
		Name: "github", ClientID: cfg.GitHubID, ClientSecret: cfg.GitHubSecret,
		AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
		Scopes: scopes,
		Fetch: func(ctx context.Context, hc *http.Client, at string) (protocol.Identity, error) {
			get := func(path string, v any) error {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
				req.Header.Set("Authorization", "Bearer "+at)
				req.Header.Set("Accept", "application/vnd.github+json")
				return doJSON(hc, req, v)
			}
			var u struct {
				ID        int64  `json:"id"`
				Login     string `json:"login"`
				Name      string `json:"name"`
				AvatarURL string `json:"avatar_url"`
			}
			if err := get("/user", &u); err != nil {
				return protocol.Identity{}, err
			}
			var emails []struct {
				Email    string `json:"email"`
				Primary  bool   `json:"primary"`
				Verified bool   `json:"verified"`
			}
			email := ""
			if get("/user/emails", &emails) == nil {
				for _, e := range emails {
					if e.Primary && e.Verified {
						email = e.Email
					}
				}
			}
			if cfg.GitHubOrg != "" {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/memberships/orgs/"+url.PathEscape(cfg.GitHubOrg), nil)
				req.Header.Set("Authorization", "Bearer "+at)
				resp, err := hc.Do(req)
				if err != nil {
					return protocol.Identity{}, err
				}
				resp.Body.Close()
				if resp.StatusCode != 200 {
					return protocol.Identity{}, fmt.Errorf("only members of %s can sign in", cfg.GitHubOrg)
				}
			}
			name := u.Name
			if name == "" {
				name = u.Login
			}
			return protocol.Identity{ID: "github:" + strconv.FormatInt(u.ID, 10), Name: name, Email: email, Provider: "github", Verified: true, Avatar: u.AvatarURL}, nil
		},
	}
}

func doJSON(hc *http.Client, req *http.Request, v any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %d %s", req.URL.Host, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
