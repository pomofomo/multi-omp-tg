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
	// ReactToMessage adds an emoji reaction to a Telegram message. Used
	// by the in-process omp extension (see internal/agent/extension) so
	// the agent can 👍 a freshly-received user message.
	ReactToMessage(chatID int64, messageID int, emoji string) error
	// SendVoiceMemo synthesises text to OGG/Opus and uploads it as a
	// Telegram voice note. Used by the omp `tg_voice` tool. Returns an
	// error when TTS is unavailable, synthesis fails, or the upload
	// fails. replyToMsgID=0 leaves the voice note unthreaded inside
	// the topic.
	SendVoiceMemo(chatID int64, threadID, replyToMsgID int, text string) error
	// RequestRestart marks the dispatcher as pending a graceful restart.
	// The dispatcher MUST drain in-flight runs and persist deferred
	// prompts before re-executing itself. callerInstanceID is the
	// claimed identity of the agent making the request (from the
	// X-Trd-Instance header) — the implementation enforces controller
	// authorisation. See DEBUG.md "Proposal C — controller-instance flag".
	RequestRestart(callerInstanceID string) error
	// PromoteController marks instanceIDOrPrefix as the controller and
	// clears the flag on every other instance. Returns the canonical
	// resolved instance id (so the CLI can print it) or an error when
	// the prefix matches multiple/zero rows.
	PromoteController(instanceIDOrPrefix string) (string, error)
	// DemoteController clears the controller flag on
	// instanceIDOrPrefix. Returns the canonical resolved instance id.
	DemoteController(instanceIDOrPrefix string) (string, error)
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
	mux.HandleFunc("POST /api/tg/react", s.handleAPIReact)
	mux.HandleFunc("POST /api/restart", s.handleAPIRestart)
	mux.HandleFunc("POST /api/instances/{id}/promote", s.handleAPIPromote)
	mux.HandleFunc("POST /api/tg/voice", s.handleAPIVoice)
	mux.HandleFunc("DELETE /api/instances/{id}/promote", s.handleAPIDemote)

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

func (s *Server) handleAPIReact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID    int64  `json:"chat_id"`
		MessageID int    `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ChatID == 0 || body.MessageID == 0 || body.Emoji == "" {
		http.Error(w, "chat_id, message_id, and emoji are required", http.StatusBadRequest)
		return
	}
	if err := s.h.ReactToMessage(body.ChatID, body.MessageID, body.Emoji); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPIRestart accepts a graceful-restart request from the in-process
// omp extension. Authorisation is delegated to the dispatcher
// (RequestRestart): the claimed instance id arrives in the
// X-Trd-Instance header and must match an instance flagged as the
// controller. Errors map to:
//
//	400 — missing header
//	403 — non-controller (unauthorized)
//	500 — any other failure
//
// On success returns 202 Accepted with a one-line plain-text body so
// curl-from-shell looks reasonable.
func (s *Server) handleAPIRestart(w http.ResponseWriter, r *http.Request) {
	caller := r.Header.Get("X-Trd-Instance")
	if caller == "" {
		http.Error(w, "X-Trd-Instance header required", http.StatusBadRequest)
		return
	}
	if err := s.h.RequestRestart(caller); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("restart accepted; draining in-flight runs\n"))
}

// ErrUnauthorized is returned by Handler.RequestRestart when the calling
// instance is not flagged as the controller. The API layer maps it to
// HTTP 403.
var ErrUnauthorized = errors.New("not authorized")

// handleAPIPromote flips the controller flag on the requested instance.
// {id} accepts the same prefix syntax as /cancel — repo-name match
// first, then instance-id prefix. Returns the canonical resolved id as
// a JSON {"instance_id":"…"} so the CLI can confirm.
func (s *Server) handleAPIPromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	resolved, err := s.h.PromoteController(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"instance_id":"` + resolved + `"}`))
}

// handleAPIDemote clears the controller flag. Same prefix semantics as
// promote. Returns the canonical id.
func (s *Server) handleAPIDemote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	resolved, err := s.h.DemoteController(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"instance_id":"` + resolved + `"}`))
}

// handleAPIVoice accepts a TTS-then-send-voice-memo request from the
// in-process omp extension. The dispatcher synthesises the text and
// uploads the resulting OGG/Opus to the requested chat/topic.
//
//	body: {"chat_id":int, "thread_id":int, "reply_to_message_id":int, "text":string}
//
// chat_id and text are required; thread_id=0 sends to General;
// reply_to_message_id=0 omits the reply binding. Maps SendVoiceMemo
// errors to HTTP 500 (TTS not configured, sherpa failure, Telegram
// upload failure all surface here so the agent can react).
func (s *Server) handleAPIVoice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID    int64  `json:"chat_id"`
		ThreadID  int    `json:"thread_id"`
		ReplyTo   int    `json:"reply_to_message_id"`
		Text      string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ChatID == 0 || body.Text == "" {
		http.Error(w, "chat_id and text are required", http.StatusBadRequest)
		return
	}
	if err := s.h.SendVoiceMemo(body.ChatID, body.ThreadID, body.ReplyTo, body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
