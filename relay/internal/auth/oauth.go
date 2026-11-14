package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	GitHubOrg    string // optional: require active membership (needs read:org)
	// SignInRequired is reported to clients so they can ask people to sign
	// in before they connect. The hub enforces it.
	SignInRequired bool
}

// endpoints are the provider URLs; tests point them at fakes.
type endpoints struct {
	googleAuth, googleToken, googleUserinfo string
	githubAuth, githubToken, githubAPI      string
}

var providerEndpoints = endpoints{
	googleAuth:     "https://accounts.google.com/o/oauth2/v2/auth",
	googleToken:    "https://oauth2.googleapis.com/token",
	googleUserinfo: "https://openidconnect.googleapis.com/v1/userinfo",
	githubAuth:     "https://github.com/login/oauth/authorize",
	githubToken:    "https://github.com/login/oauth/access_token",
	githubAPI:      "https://api.github.com",
}

const (
	pendingTTL = 10 * time.Minute
	grantTTL   = 2 * time.Minute
	maxPending = 10_000 // sign-ins in flight; beyond this /start answers 503
)

// Service runs the relay-mediated OAuth flow:
//
//  1. Client opens /v1/auth/{provider}/start?redirect_uri=knock://auth&state=S&code_challenge=C&code_challenge_method=S256
//  2. Relay redirects to the provider; the provider calls back the relay.
//  3. Relay redirects to redirect_uri?code=G&state=S with a one-time grant.
//  4. Client POSTs /v1/auth/token {code: G, code_verifier} and gets an access
//     token (1 h) plus a refresh token (90 d, sliding) to keep in the Keychain.
//
// PKCE between the client and the relay means a stolen grant is useless.
// Provider secrets stay on the relay, and provider tokens are dropped as
// soon as the identity is read.
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
	provider  string
	challenge string
	expires   time.Time
}

// userError is a sign-in failure the person can act on; its text is shown
// to them. Anything else shows a generic message.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func NewService(cfg Config, signer *Signer) *Service {
	s := newService(cfg, signer, providerEndpoints)
	go s.sweep()
	return s
}

func newService(cfg Config, signer *Signer, ep endpoints) *Service {
	s := &Service{cfg: cfg, signer: signer, providers: map[string]*Provider{},
		hc: &http.Client{Timeout: 15 * time.Second}, pending: map[string]pendingAuth{}, grants: map[string]grant{}}
	if cfg.GoogleID != "" {
		s.providers["google"] = googleProvider(cfg, ep)
	}
	if cfg.GitHubID != "" {
		s.providers["github"] = githubProvider(cfg, ep)
	}
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

// policy names the restriction a provider's sign-ins were checked against.
// Tokens carry it, so tightening or changing the restriction signs people out.
func (s *Service) policy(provider string) string {
	switch {
	case provider == "google" && s.cfg.GoogleDomain != "":
		return "hd:" + strings.ToLower(s.cfg.GoogleDomain)
	case provider == "github" && s.cfg.GitHubOrg != "":
		return "org:" + strings.ToLower(s.cfg.GitHubOrg)
	}
	return ""
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/auth/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, protocol.AuthProviders{Providers: s.Providers(), SignInRequired: s.cfg.SignInRequired})
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
	method := q.Get("code_challenge_method")
	// S256 only: a base64url SHA-256 is 43 characters.
	if !validRedirect(redirect) || clientState == "" || len(clientState) > 512 || len(challenge) != 43 ||
		(method != "" && method != "S256") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	state := RandomString(24)
	s.mu.Lock()
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		http.Error(w, "too many sign-ins in progress, try again in a minute", http.StatusServiceUnavailable)
		return
	}
	s.pending[state] = pendingAuth{provider: p.Name, redirect: redirect, clientState: clientState, challenge: challenge,
		expires: time.Now().Add(pendingTTL)}
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
			v.Set("hd", s.cfg.GoogleDomain) // a hint only; the callback checks the claim
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
		http.Error(w, "This sign-in expired. Start it again from Knock.", http.StatusBadRequest)
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
		msg := "The sign-in didn't finish. Try again."
		if e == "access_denied" {
			msg = "You didn't allow the sign-in."
		}
		back(url.Values{"error": {"access_denied"}, "error_description": {msg}})
		return
	}
	p := s.providers[pa.provider]
	id, err := s.exchange(r.Context(), p, q.Get("code"))
	if err != nil {
		msg := "Sign-in didn't work. Try again."
		var ue userError
		if errors.As(err, &ue) {
			msg = ue.msg
		}
		slog.Warn("sign-in failed", "provider", p.Name, "err", err)
		back(url.Values{"error": {"sign_in_failed"}, "error_description": {msg}})
		return
	}
	code := RandomString(24)
	s.mu.Lock()
	s.grants[code] = grant{id: id, provider: p.Name, challenge: pa.challenge, expires: time.Now().Add(grantTTL)}
	s.mu.Unlock()
	slog.Info("signed in", "provider", p.Name, "user", id.ID)
	back(url.Values{"code": {code}})
}

func (s *Service) exchange(ctx context.Context, p *Provider, code string) (protocol.Identity, error) {
	if code == "" {
		return protocol.Identity{}, errors.New("provider sent no code")
	}
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
		return protocol.Identity{}, fmt.Errorf("token exchange: %s", clean(tok.Error, 64))
	}
	id, err := p.Fetch(ctx, s.hc, tok.AccessToken)
	if err != nil {
		return protocol.Identity{}, err
	}
	return cleanIdentity(id), nil
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
	pol := s.policy(id.Provider)
	at, aexp := s.signer.IssueWithPolicy(KindAccess, id, pol, AccessTTL)
	rt, rexp := s.signer.IssueWithPolicy(KindRefresh, id, pol, RefreshTTL)
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
	delete(s.grants, in.Code) // one try per grant, right or wrong
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(in.Verifier))
	if !ok || time.Now().After(g.expires) || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	if _, ok := s.providers[g.provider]; !ok {
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
	// The provider must still be offered, under the same restriction the
	// user passed when they signed in.
	if _, ok := s.providers[c.Provider]; !ok || c.Policy != s.policy(c.Provider) {
		slog.Info("refresh refused, sign-in settings changed", "provider", c.Provider, "user", c.Subject)
		writeJSON(w, 401, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(w, 200, s.issue(c.Identity()))
}

func (s *Service) sweep() {
	for range time.Tick(time.Minute) {
		s.expire(time.Now())
	}
}

func (s *Service) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
}

func googleProvider(cfg Config, ep endpoints) *Provider {
	return &Provider{
		Name: "google", ClientID: cfg.GoogleID, ClientSecret: cfg.GoogleSecret,
		AuthURL: ep.googleAuth, TokenURL: ep.googleToken,
		Scopes: "openid email profile",
		Fetch: func(ctx context.Context, hc *http.Client, at string) (protocol.Identity, error) {
			var u struct {
				Sub           string `json:"sub"`
				Name          string `json:"name"`
				Email         string `json:"email"`
				Picture       string `json:"picture"`
				HD            string `json:"hd"`
				EmailVerified bool   `json:"email_verified"`
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.googleUserinfo, nil)
			req.Header.Set("Authorization", "Bearer "+at)
			if err := doJSON(hc, req, &u); err != nil {
				return protocol.Identity{}, err
			}
			if u.Sub == "" || u.Email == "" || !u.EmailVerified {
				return protocol.Identity{}, userError{"Your Google account needs a verified email to sign in."}
			}
			// hd is set by Google only for Workspace accounts; the start
			// URL's hd parameter is just a hint the user can strip.
			if cfg.GoogleDomain != "" && !strings.EqualFold(u.HD, cfg.GoogleDomain) {
				return protocol.Identity{}, userError{"Only " + cfg.GoogleDomain + " Google accounts can sign in to this relay."}
			}
			if u.Name == "" {
				u.Name = u.Email
			}
			return protocol.Identity{ID: "google:" + u.Sub, Name: u.Name, Email: u.Email, Provider: "google", Verified: true, Avatar: u.Picture}, nil
		},
	}
}

func githubProvider(cfg Config, ep endpoints) *Provider {
	scopes := "read:user user:email"
	if cfg.GitHubOrg != "" {
		scopes += " read:org"
	}
	return &Provider{
		Name: "github", ClientID: cfg.GitHubID, ClientSecret: cfg.GitHubSecret,
		AuthURL: ep.githubAuth, TokenURL: ep.githubToken,
		Scopes: scopes,
		Fetch: func(ctx context.Context, hc *http.Client, at string) (protocol.Identity, error) {
			get := func(path string, v any) error {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.githubAPI+path, nil)
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
			if u.ID == 0 || u.Login == "" {
				return protocol.Identity{}, errors.New("github: user has no id")
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
				// Active membership only: an invitation the user hasn't
				// accepted comes back 200 with state "pending".
				var m struct {
					State string `json:"state"`
				}
				err := get("/user/memberships/orgs/"+url.PathEscape(cfg.GitHubOrg), &m)
				if err != nil || m.State != "active" {
					slog.Info("github org check failed", "user", "github:"+strconv.FormatInt(u.ID, 10), "state", m.State, "err", err)
					return protocol.Identity{}, userError{"Only members of the " + cfg.GitHubOrg + " GitHub organization can sign in to this relay. " +
						"If you are one, an org owner may need to approve this relay's GitHub app."}
				}
			}
			name := u.Name
			if name == "" {
				name = u.Login
			}
			return protocol.Identity{ID: "github:" + strconv.FormatInt(u.ID, 10), Name: name, Email: email, Login: u.Login,
				Provider: "github", Verified: true, Avatar: u.AvatarURL}, nil
		},
	}
}

// cleanIdentity keeps provider text safe to print in a terminal or alert.
func cleanIdentity(id protocol.Identity) protocol.Identity {
	id.Name = clean(id.Name, 80)
	id.Email = clean(id.Email, 254)
	id.Login = clean(id.Login, 39)
	if !strings.HasPrefix(id.Avatar, "https://") || len(id.Avatar) > 512 {
		id.Avatar = ""
	}
	if id.Name == "" {
		id.Name = id.Email
	}
	return id
}

func clean(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// doJSON decodes a 2xx JSON body. Errors carry the host, status and the
// provider's short error code, never the body: it could echo a token.
func doJSON(hc *http.Client, req *http.Request, v any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err // URLs here never carry tokens
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error any `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		code, _ := e.Error.(string)
		return fmt.Errorf("%s: %d %s", req.URL.Host, resp.StatusCode, clean(code, 64))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
