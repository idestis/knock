// knockd is the Knock relay. It brokers knocks and forwards session bytes;
// it never runs a shell. Deployed as knock-relay.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idestis/knock/protocol"
	"github.com/idestis/knock/relay/internal/auth"
	"github.com/idestis/knock/relay/internal/hub"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(msg string) {
	slog.Error(msg)
	os.Exit(1)
}

func main() {
	addr := flag.String("addr", env("KNOCK_ADDR", ":8787"), "listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	secret := []byte(os.Getenv("KNOCK_TOKEN_SECRET"))
	if len(secret) < 32 {
		slog.Warn("KNOCK_TOKEN_SECRET unset or shorter than 32 bytes; using a random one, sign-ins and door claims will not survive a restart")
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	signer := auth.NewSigner(secret)
	authCfg := auth.Config{
		PublicURL:    env("KNOCK_PUBLIC_URL", "http://127.0.0.1:8787"),
		GoogleID:     os.Getenv("KNOCK_GOOGLE_CLIENT_ID"),
		GoogleSecret: os.Getenv("KNOCK_GOOGLE_CLIENT_SECRET"),
		GoogleDomain: strings.TrimSpace(os.Getenv("KNOCK_GOOGLE_DOMAIN")),
		GitHubID:     os.Getenv("KNOCK_GITHUB_CLIENT_ID"),
		GitHubSecret: os.Getenv("KNOCK_GITHUB_CLIENT_SECRET"),
		GitHubOrg:    strings.TrimSpace(os.Getenv("KNOCK_GITHUB_ORG")),
	}
	providers := 0
	for _, p := range [][2]string{{authCfg.GoogleID, authCfg.GoogleSecret}, {authCfg.GitHubID, authCfg.GitHubSecret}} {
		if p[0] != "" && p[1] == "" {
			fatal("a sign-in client ID is set without its secret (KNOCK_*_CLIENT_SECRET)")
		}
		if p[0] != "" {
			providers++
		}
	}
	allowAnon, err := allowAnonymous(os.Getenv("KNOCK_ALLOW_ANONYMOUS"), providers)
	if err != nil {
		fatal(err.Error())
	}
	if providers > 0 && !strings.HasPrefix(authCfg.PublicURL, "https://") {
		slog.Warn("KNOCK_PUBLIC_URL is not https; providers will only accept it for local testing", "public_url", authCfg.PublicURL)
	}
	authCfg.SignInRequired = !allowAnon
	// /start limits per address the same way the hub does; h is set below,
	// before the server takes requests.
	var h *hub.Hub
	authCfg.ClientIP = func(r *http.Request) string { return h.ClientIP(r) }
	authSvc := auth.NewService(authCfg, signer)

	timeout, _ := strconv.Atoi(env("KNOCK_KNOCK_TIMEOUT_SECONDS", "60"))
	// Unset trusts no proxy: X-Forwarded-For is ignored and limits key on
	// the socket's address. Behind an HTTP proxy, list its addresses here,
	// or every anonymous knocker shares the proxy's address and its limits.
	proxies, err := hub.ParseTrustedProxies(os.Getenv("KNOCK_TRUSTED_PROXIES"))
	if err != nil {
		fatal(err.Error())
	}
	ttl, err := doorTTL(os.Getenv("KNOCK_DOOR_TTL"))
	if err != nil {
		fatal(err.Error())
	}
	// KNOCK_VALKEY_URL is a secret (it can carry a password): never logged.
	doors, err := openStore(os.Getenv("KNOCK_STORE"), os.Getenv("KNOCK_VALKEY_URL"), ttl)
	if err != nil {
		fatal(err.Error())
	}
	defer doors.Close()
	h = hub.New(hub.Config{
		AllowAnonymous: allowAnon,
		KnockTimeout:   time.Duration(timeout) * time.Second,
		Signer:         signer,
		Providers:      authSvc.Providers(),
		RelayName:      env("KNOCK_RELAY_NAME", "knock-relay"),
		TrustedProxies: proxies,
		DoorTTL:        ttl,
		Store:          doors,
	})

	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", h)
	authSvc.Register(mux)
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": version, "protocol": protocol.Version, "allows_anonymous": allowAnon, "sign_in_required": !allowAnon,
			"auth_providers": authSvc.Providers(),
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for k, v := range h.Stats() {
			_, _ = w.Write([]byte("knock_" + k + " " + strconv.Itoa(v) + "\n"))
		}
	})

	srv := &http.Server{
		Addr: *addr, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second, // idle keep-alives; WebSockets ping on their own
		MaxHeaderBytes:    8 << 10,          // room for OAuth callback query strings, not much more
	}
	go func() {
		slog.Info("knockd listening", "addr", *addr, "version", version, "anonymous", allowAnon, "providers", authSvc.Providers(), "trusted_proxies", len(proxies), "store", env("KNOCK_STORE", "memory"), "door_ttl", ttl.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	grace, _ := time.ParseDuration(env("KNOCK_DRAIN_GRACE", "25s"))
	slog.Info("draining", "grace", grace.String())
	h.Drain()
	deadline := time.Now().Add(grace)
	for h.LiveSessions() > 0 && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	h.CloseAll()
}
