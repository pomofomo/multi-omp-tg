// Package api is the dispatcher's small HTTP control plane.
//
// It serves three things and three things only:
//
//   GET    /healthz                  — liveness probe
//   GET    /api/instances            — list instances (JSON, runtime-enriched)
//   GET    /api/allowed              — allowlist (JSON array of usernames)
//   POST   /api/allowed/{username}   — add to allowlist
//   DELETE /api/allowed/{username}   — remove from allowlist
//   POST   /api/instances/{id}/cancel — interrupt the in-flight agent run
//
// There is no WebSocket and no channel plugin in the headless-omp port.
// Each Telegram message spawns a one-shot `omp -p` subprocess; nothing
// persistent connects back to the dispatcher.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Handler is the interface the dispatcher implements so this server can
// answer the HTTP endpoints. It deliberately omits every WS-era method
// (Register/Unregister/OnOutbound/AuthSecret).
type Handler interface {
	// ListInstances returns JSON-encoded instance list for the CLI API.
	ListInstances() ([]byte, error)
	// AllowedUsers returns the stored allowlist.
	AllowedUsers() ([]string, error)
	// AddAllowedUser adds a username to the allowlist.
	AddAllowedUser(username string) error
	// RemoveAllowedUser removes a username from the allowlist.
	RemoveAllowedUser(username string) error
	// CancelRun interrupts any in-flight agent invocation for the given
	// instance id (or prefix). Returns nil if there was nothing to cancel.
	CancelRun(instanceIDOrPrefix string) error
}

// Server serves the dispatcher's HTTP endpoints.
type Server struct {
	addr   string
	logger *slog.Logger
	h      Handler
	srv    *http.Server
}

// New constructs a Server bound to addr (e.g. "127.0.0.1:7777").
func New(addr string, logger *slog.Logger, h Handler) *Server {
	return &Server{addr: addr, logger: logger, h: h}
}

// ListenAndServe starts the HTTP listener. Blocks until ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/instances", s.handleAPIInstances)
	mux.HandleFunc("GET /api/allowed", s.handleAPIAllowedList)
	mux.HandleFunc("POST /api/allowed/{username}", s.handleAPIAllowedAdd)
	mux.HandleFunc("DELETE /api/allowed/{username}", s.handleAPIAllowedRemove)
	mux.HandleFunc("POST /api/instances/{id}/cancel", s.handleAPIInstanceCancel)

	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Addr returns the listen address (useful in tests).
func (s *Server) Addr() string { return s.addr }

func (s *Server) handleAPIInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := s.h.ListInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) handleAPIAllowedList(w http.ResponseWriter, _ *http.Request) {
	users, err := s.h.AllowedUsers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if users == nil {
		users = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(users)
	_, _ = w.Write(data)
}

func (s *Server) handleAPIAllowedAdd(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if username == "" {
		http.Error(w, "username required", http.StatusBadRequest)
		return
	}
	if err := s.h.AddAllowedUser(username); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIAllowedRemove(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if username == "" {
		http.Error(w, "username required", http.StatusBadRequest)
		return
	}
	if err := s.h.RemoveAllowedUser(username); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIInstanceCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := s.h.CancelRun(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
