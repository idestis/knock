package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"time"
)

// LoginOptions configure a browser sign-in.
type LoginOptions struct {
	Relay    string
	Provider string
	HTTP     *http.Client
	// Open shows the URL to the user (opens the browser). Required.
	Open func(url string) error
	// Timeout for the whole flow; default 5 minutes.
	Timeout time.Duration
}

// PKCE returns a verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomState() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type callbackResult struct {
	code string
	err  error
}

// Login runs the relay-mediated OAuth flow through a loopback redirect on
// 127.0.0.1 and returns the tokens. It does not save them.
func Login(ctx context.Context, opt LoginOptions) (Tokens, error) {
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Tokens{}, fmt.Errorf("start the sign-in listener: %w", err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	verifier, challenge := PKCE()
	state := randomState()

	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "This sign-in link doesn't match. Run knock login again.", http.StatusBadRequest)
			return
		}
		res := callbackResult{code: q.Get("code")}
		switch {
		case q.Get("error") != "":
			msg := q.Get("error_description")
			if msg == "" {
				msg = q.Get("error")
			}
			res.err = fmt.Errorf("sign-in didn't finish: %s", msg)
		case res.code == "":
			res.err = errors.New("sign-in didn't finish: the relay sent no code")
		}
		writePage(w, res.err)
		select {
		case results <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	}()

	v := url.Values{}
	v.Set("redirect_uri", redirect)
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	start := opt.Relay + "/v1/auth/" + url.PathEscape(opt.Provider) + "/start?" + v.Encode()
	if err := opt.Open(start); err != nil {
		return Tokens{}, err
	}

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Tokens{}, errors.New("sign-in timed out. Run knock login to try again")
		}
		return Tokens{}, ctx.Err()
	case res := <-results:
		if res.err != nil {
			return Tokens{}, res.err
		}
		return Exchange(ctx, opt.HTTP, opt.Relay, res.code, verifier)
	}
}

func writePage(w http.ResponseWriter, err error) {
	title, body := "You're signed in", "You can close this tab and go back to your terminal."
	if err != nil {
		title, body = "Sign-in didn't work", html.EscapeString(err.Error())+" You can close this tab."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Knock</title>
<style>body{font:16px -apple-system,system-ui,sans-serif;display:flex;min-height:100vh;margin:0;align-items:center;justify-content:center;background:#f5f6f8;color:#111}
main{text-align:center}h1{font-size:22px;margin:0 0 8px}p{color:#555;margin:0}.k{color:#0043F3;font-weight:700;font-size:28px;margin-bottom:16px}
@media(prefers-color-scheme:dark){body{background:#111;color:#eee}p{color:#aaa}}</style></head>
<body><main><div class="k">knock</div><h1>%s</h1><p>%s</p></main></body></html>`, title, body)
}
