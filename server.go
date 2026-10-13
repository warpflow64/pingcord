package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

type server struct {
	cfg     HTTPConfig
	monitor *Monitor
	deadman *Deadman
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", s.auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"targets": s.monitor.Snapshot(),
			"deadman": s.deadman.Snapshot(),
		})
	}))
	mux.HandleFunc("/ping/{name}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !s.deadman.Ping(r.PathValue("name")) {
			http.Error(w, "unknown deadman check", http.StatusNotFound)
			return
		}
		w.Write([]byte("ok\n"))
	}))
	return mux
}

// auth は http.token が設定されている場合に ?token= か Authorization: Bearer を要求する。
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got := r.URL.Query().Get("token")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// listen は起動時にポートのバインド失敗を検出できるよう、Listen だけ先に行う。
func (s *server) listen() (net.Listener, error) {
	return net.Listen("tcp", s.cfg.Listen)
}

func (s *server) serve(ctx context.Context, ln net.Listener) {
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.Info("http server listening", "addr", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("http server", "err", err)
	}
}
