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

func main() {
	addr := flag.String("addr", env("KNOCK_ADDR", ":8787"), "listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	secret := []byte(os.Getenv("KNOCK_TOKEN_SECRET"))
	if len(secret) < 32 {
		slog.Warn("KNOCK_TOKEN_SECRET unset or shorter than 32 bytes; using a random one, sign-ins will not survive a restart")
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	signer := auth.NewSigner(secret)
	authSvc := auth.NewService(auth.Config{
		PublicURL:    env("KNOCK_PUBLIC_URL", "http://127.0.0.1:8787"),
		GoogleID:     os.Getenv("KNOCK_GOOGLE_CLIENT_ID"),
		GoogleSecret: os.Getenv("KNOCK_GOOGLE_CLIENT_SECRET"),
		GoogleDomain: os.Getenv("KNOCK_GOOGLE_DOMAIN"),
		GitHubID:     os.Getenv("KNOCK_GITHUB_CLIENT_ID"),
		GitHubSecret: os.Getenv("KNOCK_GITHUB_CLIENT_SECRET"),
		GitHubOrg:    os.Getenv("KNOCK_GITHUB_ORG"),
	}, signer)

	allowAnon, _ := strconv.ParseBool(env("KNOCK_ALLOW_ANONYMOUS", "true"))
	timeout, _ := strconv.Atoi(env("KNOCK_KNOCK_TIMEOUT_SECONDS", "60"))
	h := hub.New(hub.Config{
		AllowAnonymous: allowAnon,
		KnockTimeout:   time.Duration(timeout) * time.Second,
		Signer:         signer,
		Providers:      authSvc.Providers(),
		RelayName:      env("KNOCK_RELAY_NAME", "knock-relay"),
	})

	mux := http.NewServeMux()
	mux.Handle("GET /v1/ws", h)
	authSvc.Register(mux)
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": version, "protocol": protocol.Version, "allows_anonymous": allowAnon, "auth_providers": authSvc.Providers(),
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

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("knockd listening", "addr", *addr, "version", version, "anonymous", allowAnon, "providers", authSvc.Providers())
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
