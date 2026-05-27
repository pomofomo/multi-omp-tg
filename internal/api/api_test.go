package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHandler struct {
	mu        sync.Mutex
	instances []byte
	allowed   []string
	addErr    error
	rmErr     error
	cancelled []string
	cancelErr error
	reactions    []reactEvent
	reactErr     error
	restartCalls []string
	restartErr   error
}

func (h *fakeHandler) ListInstances() ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instances == nil {
		return []byte("[]"), nil
	}
	return h.instances, nil
}

func (h *fakeHandler) AllowedUsers() ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.allowed))
	copy(out, h.allowed)
	return out, nil
}

func (h *fakeHandler) AddAllowedUser(u string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.addErr != nil {
		return h.addErr
	}
	h.allowed = append(h.allowed, u)
	return nil
}

func (h *fakeHandler) RemoveAllowedUser(u string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rmErr != nil {
		return h.rmErr
	}
	out := h.allowed[:0]
	for _, v := range h.allowed {
		if v != u {
			out = append(out, v)
		}
	}
	h.allowed = out
	return nil
}

func (h *fakeHandler) CancelRun(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancelErr != nil {
		return h.cancelErr
	}
	h.cancelled = append(h.cancelled, id)
	return nil
}

type reactEvent struct {
	chatID    int64
	messageID int
	emoji     string
}

func (h *fakeHandler) ReactToMessage(chatID int64, messageID int, emoji string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reactErr != nil {
		return h.reactErr
	}
	h.reactions = append(h.reactions, reactEvent{chatID: chatID, messageID: messageID, emoji: emoji})
	return nil
}

func (h *fakeHandler) RequestRestart(callerInstanceID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restartErr != nil {
		return h.restartErr
	}
	h.restartCalls = append(h.restartCalls, callerInstanceID)
	return nil
}

func (h *fakeHandler) PromoteController(q string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return q, nil
}

func (h *fakeHandler) DemoteController(q string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return q, nil
}

// startTestServer spins up the api.Server on a random free port and
// returns its base URL and a cancel that shuts it down.
func startTestServer(t *testing.T, h Handler) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(addr, logger, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.ListenAndServe(ctx)
		close(done)
	}()

	// Wait for the server to become reachable.
	deadline := time.Now().Add(2 * time.Second)
	base := "http://" + addr
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	return base, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func TestHealthz(t *testing.T) {
	base, stop := startTestServer(t, &fakeHandler{})
	defer stop()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body: %q", body)
	}
}

func TestInstancesGET(t *testing.T) {
	h := &fakeHandler{instances: []byte(`[{"instance_id":"x"}]`)}
	base, stop := startTestServer(t, h)
	defer stop()

	resp, err := http.Get(base + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"instance_id":"x"`) {
		t.Errorf("body: %s", body)
	}
}

func TestInstancesMethodNotAllowed(t *testing.T) {
	base, stop := startTestServer(t, &fakeHandler{})
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/instances", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestAllowedAddRemoveList(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	// Add.
	req, _ := http.NewRequest(http.MethodPost, base+"/api/allowed/alice", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("add status: %d", resp.StatusCode)
	}

	// List.
	resp, err = http.Get(base + "/api/allowed")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list []string
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(list) != 1 || list[0] != "alice" {
		t.Errorf("got %v", list)
	}

	// Remove.
	req, _ = http.NewRequest(http.MethodDelete, base+"/api/allowed/alice", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove status: %d", resp.StatusCode)
	}
}

func TestAllowedAddErrorPropagates(t *testing.T) {
	h := &fakeHandler{addErr: errors.New("nope")}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/allowed/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "nope") {
		t.Errorf("body: %s", body)
	}
}

func TestInstanceCancel(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/instances/abc-123/cancel", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(h.cancelled) != 1 || h.cancelled[0] != "abc-123" {
		t.Errorf("cancelled: %v", h.cancelled)
	}
}

func TestInstanceCancelError(t *testing.T) {
	h := &fakeHandler{cancelErr: errors.New("no such run")}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/instances/missing/cancel", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestReactEndpoint(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	body := strings.NewReader(`{"chat_id":42,"message_id":7,"emoji":"👍"}`)
	resp, err := http.Post(base+"/api/tg/react", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: %d body: %s", resp.StatusCode, raw)
	}
	if len(h.reactions) != 1 {
		t.Fatalf("reactions: %v", h.reactions)
	}
	got := h.reactions[0]
	if got.chatID != 42 || got.messageID != 7 || got.emoji != "👍" {
		t.Errorf("react payload: %+v", got)
	}
}

func TestReactEndpointValidatesBody(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	cases := []struct {
		name string
		body string
	}{
		{"missing emoji", `{"chat_id":1,"message_id":2}`},
		{"missing chat_id", `{"message_id":2,"emoji":"👍"}`},
		{"missing message_id", `{"chat_id":1,"emoji":"👍"}`},
		{"invalid json", `not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(base+"/api/tg/react", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status: want 400, got %d", resp.StatusCode)
			}
		})
	}
	if len(h.reactions) != 0 {
		t.Errorf("handler was called with invalid payloads: %v", h.reactions)
	}
}

func TestReactEndpointPropagatesError(t *testing.T) {
	h := &fakeHandler{reactErr: errors.New("telegram down")}
	base, stop := startTestServer(t, h)
	defer stop()

	body := strings.NewReader(`{"chat_id":1,"message_id":2,"emoji":"👍"}`)
	resp, err := http.Post(base+"/api/tg/react", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestRestartEndpointAccepted(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/restart", nil)
	req.Header.Set("X-Trd-Instance", "ctrl-inst-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: want 202, got %d", resp.StatusCode)
	}
	if len(h.restartCalls) != 1 || h.restartCalls[0] != "ctrl-inst-id" {
		t.Errorf("RequestRestart calls: %v", h.restartCalls)
	}
}

func TestRestartEndpointRejectsMissingHeader(t *testing.T) {
	h := &fakeHandler{}
	base, stop := startTestServer(t, h)
	defer stop()

	resp, err := http.Post(base+"/api/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
	if len(h.restartCalls) != 0 {
		t.Errorf("handler was called: %v", h.restartCalls)
	}
}

func TestRestartEndpointUnauthorizedMapsTo403(t *testing.T) {
	h := &fakeHandler{restartErr: ErrUnauthorized}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/restart", nil)
	req.Header.Set("X-Trd-Instance", "not-the-controller")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", resp.StatusCode)
	}
}

func TestRestartEndpointPropagatesError(t *testing.T) {
	h := &fakeHandler{restartErr: errors.New("storage failure")}
	base, stop := startTestServer(t, h)
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, base+"/api/restart", nil)
	req.Header.Set("X-Trd-Instance", "ctrl")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", resp.StatusCode)
	}
}
