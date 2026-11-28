package main

// admin.go: the management-plane seam.
//
// TODO-ASSEMBLY — admin.NewProduction(cfg, deps) does not exist yet (another
// agent is implementing it). This file is the ONLY place the assembly touches
// the management plane, so the swap is one function:
//
//	// when admin.NewProduction lands:
//	srv, err := admin.NewProduction(admin.Config{ListenAddr: cfg.AdminListen, ...},
//	    admin.Deps{DB: adminDB, AuditJournal: <cold journal>, Bridge: ...})
//	return srv, err
//
// What IS wired here (and is genuinely useful today):
//   - POST /admin/inject  → the bridge's AdminAPI frame injector, the single
//     non-socket entry into the bus (bridge/adminapi already implements it).
//   - GET  /healthz       → liveness of the admin listener.
//
// What is NOT wired: the RBAC REST layer under /api/admin/*. It answers
// 501 NOT_IMPLEMENTED with an explicit code so callers cannot mistake it for a
// working endpoint. The audit journal it requires is a cold-store component
// (hash-chained, Postgres); fabricating one in the assembler would violate the
// audit red line.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

type adminPlane struct {
	// addr is written once by Run (after net.Listen resolves ":0") and read by
	// Addr()/logging from other goroutines — guarded by mu (CI -race proved it).
	mu   sync.RWMutex
	addr string
	srv  *http.Server
	lg   *log.Logger
}

// newAdminPlane builds the admin HTTP surface (listener created in Run so a
// :0 port is usable in tests).
func newAdminPlane(addr string, injector http.Handler, lg *log.Logger) (*adminPlane, error) {
	if addr == "" {
		return nil, errors.New("chatd: admin_listen is empty")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("chatd: invalid admin_listen %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		// docs/deploy.md: the admin surface is loopback-only; Caddy is the only
		// way in (and it only proxies /api/admin/* for whitelisted IPs).
		return nil, fmt.Errorf("chatd: admin_listen %q must be loopback", addr)
	}

	mux := http.NewServeMux()
	if injector != nil {
		mux.Handle("POST /admin/inject", injector)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusOK, map[string]any{"ok": true})
	})
	// The RBAC REST layer lives on admin_rest_listen (a separate loopback
	// listener) and reaches the bus through this injector. Anything else under
	// /api/admin/ on the injector port is a misrouted request.
	mux.HandleFunc("/api/admin/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{
			"error": "admin REST is served on admin_rest_listen",
			"code":  "WRONG_SURFACE",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return &adminPlane{
		addr: addr,
		lg:   lg,
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}, nil
}

// Run listens and serves until ctx is canceled.
func (a *adminPlane) Run(ctx context.Context) error {
	a.mu.RLock()
	listenAddr := a.addr
	a.mu.RUnlock()
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("chatd: admin listen %s: %w", a.addr, err)
	}
	bound := ln.Addr().String()
	a.mu.Lock()
	a.addr = bound
	a.mu.Unlock()
	if a.lg != nil {
		a.lg.Info("admin surface listening", "addr", bound,
			"inject_api", true, "rest_layer", "admin_rest_listen")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- a.srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.srv.Shutdown(sctx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Addr returns the bound address (valid after Run starts listening).
func (a *adminPlane) Addr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.addr
}

// Close stops the listener immediately (drain path).
func (a *adminPlane) Close() error { return a.srv.Close() }

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
