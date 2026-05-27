package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pomofomo/multi-claude-tg/internal/agent"
	"github.com/pomofomo/multi-claude-tg/internal/storage"
	"github.com/pomofomo/multi-claude-tg/internal/telegram"
)

// fakeRunner records every Start call and returns a fakeHandle whose
// event stream the test controls. Lets us assert routing behaviour (argv,
// queueing, session capture, cancel) without a real omp binary.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []agent.RunOptions
	pending []*fakeHandle
	// emit is what each new handle should emit before EvDone. Tests
	// mutate this between calls when they want different content per turn.
	emit []agent.Event
}

func (r *fakeRunner) Start(_ context.Context, opts agent.RunOptions) (agentHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, opts)
	events := make([]agent.Event, len(r.emit))
	copy(events, r.emit)
	h := newFakeHandle(events)
	r.pending = append(r.pending, h)
	return h, nil
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRunner) lastCall() agent.RunOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return agent.RunOptions{}
	}
	return r.calls[len(r.calls)-1]
}

// pendingAt returns the i-th handle ever produced (in spawn order).
func (r *fakeRunner) pendingAt(i int) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.pending) {
		return nil
	}
	return r.pending[i]
}

type fakeHandle struct {
	events    chan agent.Event
	released  chan struct{}
	once      sync.Once
	cancelled atomic.Bool
}

func newFakeHandle(initial []agent.Event) *fakeHandle {
	h := &fakeHandle{
		events:   make(chan agent.Event, 16),
		released: make(chan struct{}),
	}
	for _, e := range initial {
		h.events <- e
	}
	go func() {
		<-h.released
		h.events <- agent.Event{Kind: agent.EvDone}
		close(h.events)
	}()
	return h
}

func (h *fakeHandle) Events() <-chan agent.Event { return h.events }

func (h *fakeHandle) Cancel(_ time.Duration) {
	h.cancelled.Store(true)
	h.release()
}

func (h *fakeHandle) release() {
	h.once.Do(func() { close(h.released) })
}

// recordingTelegram is a stand-in for *telegram.Client. It collects every
// outbound message and reaction so tests can assert (or just ignore) what
// the dispatcher tried to send.
type recordingTelegram struct {
	mu        sync.Mutex
	sent      []telegram.SendMessageParams
	reactions []reaction
}

type reaction struct {
	chatID int64
	msgID  int
	emoji  string
}

func (r *recordingTelegram) sendMessage(_ context.Context, p telegram.SendMessageParams) (telegram.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, p)
	return telegram.Message{MessageID: 9999}, nil
}

func (r *recordingTelegram) setReaction(_ context.Context, chatID int64, msgID int, emoji string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reactions = append(r.reactions, reaction{chatID: chatID, msgID: msgID, emoji: emoji})
	return nil
}

func (r *recordingTelegram) sentTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.sent))
	for i, p := range r.sent {
		out[i] = p.Text
	}
	return out
}

// newTestDispatcher returns a Dispatcher wired against an empty bbolt
// store, the supplied fakeRunner, and a recordingTelegram for outbound
// calls. The real telegram client, media engine, and api server are not
// initialized — callers MUST only exercise routing/queue/cancel paths.
func newTestDispatcher(t *testing.T, r *fakeRunner) (*Dispatcher, *recordingTelegram) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(dir + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rec := &recordingTelegram{}
	d := &Dispatcher{
		opts: Options{
			Logger:     discardLogger(),
			RunTimeout: 30 * time.Second,
		},
		logger:       discardLogger(),
		store:        store,
		runner:       r,
		sendMessage:  rec.sendMessage,
		setReaction:  rec.setReaction,
		runs:         map[string]*agentRun{},
		pendingQueue: map[string][]queuedPrompt{},
	}
	return d, rec
}

// drainHandle releases the fake run so the dispatcher's driveAgentRun
// goroutine can finalize. Does NOT wait for the runs map to clear,
// because finishRun may immediately dispatch the next queued prompt —
// the map is only momentarily empty. Callers that need post-drain
// invariants should waitFor() on the concrete property they care about.
func drainHandle(t *testing.T, _ *Dispatcher, _ string, h *fakeHandle) {
	t.Helper()
	h.release()
}

func TestEnqueueOrRunSpawnsImmediatelyWhenIdle(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvSessionID, SessionID: "sess-xyz"},
		{Kind: agent.EvAssistantFinal, Text: "reply text"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "i1", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	if err := d.store.Put(inst); err != nil {
		t.Fatal(err)
	}

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 10, text: "hello"})

	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	got := r.lastCall()
	if got.Cwd != inst.RepoPath {
		t.Errorf("Cwd: want %q, got %q", inst.RepoPath, got.Cwd)
	}
	if got.Prompt != "hello" {
		t.Errorf("Prompt: got %q", got.Prompt)
	}
	if got.SessionID != "" {
		t.Errorf("SessionID: first call must not pass --resume; got %q", got.SessionID)
	}

	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))

	// driveAgentRun finalizes asynchronously after the channel closes;
	// wait for the reply to land in our recorder.
	waitFor(t, 2*time.Second, func() bool { return len(rec.sentTexts()) == 1 })

	updated, _ := d.store.Get(inst.InstanceID)
	if updated == nil || updated.SessionID != "sess-xyz" {
		t.Fatalf("session id not persisted: %+v", updated)
	}
	if rec.sentTexts()[0] != "reply text" {
		t.Errorf("expected one reply 'reply text', got %v", rec.sentTexts())
	}
}

func TestEnqueueOrRunResumesWhenSessionPresent(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "ok"}}}
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{
		InstanceID: "i2", ChatID: 1, TopicID: 1,
		RepoPath:  t.TempDir(),
		State:     storage.StateRunning,
		SessionID: "previous-session",
	}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "again"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	if got := r.lastCall().SessionID; got != "previous-session" {
		t.Errorf("SessionID: want previous-session, got %q", got)
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
}

func TestQueueWhileBusy(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "done"}}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "i3", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	// First prompt — spawns immediately.
	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "first"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	// Second prompt — must queue (no second Start while first is in flight).
	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 2, text: "second"})

	// Reaction should fire on queue (best-effort, async).
	waitFor(t, 1*time.Second, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.reactions) >= 1
	})

	// Give the dispatcher a moment to (NOT) spawn another runner.
	time.Sleep(100 * time.Millisecond)
	if got := r.callCount(); got != 1 {
		t.Fatalf("queued prompt should not spawn yet; calls=%d", got)
	}

	// Release the first run; the queue should drain and spawn the second.
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 2 })

	if got := r.lastCall().Prompt; got != "second" {
		t.Errorf("queued prompt content: want %q, got %q", "second", got)
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(1))
}

func TestCancelRunInterruptsActive(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "done"}}}
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "abc-12345", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning, RepoName: "myrepo"}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "long task"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	if err := d.CancelRun("myrepo"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	if !r.pendingAt(0).cancelled.Load() {
		t.Error("handle should have been cancelled")
	}

	waitFor(t, 2*time.Second, func() bool {
		d.runMu.Lock()
		defer d.runMu.Unlock()
		return len(d.runs) == 0
	})
}

func TestCancelRunNoopWhenIdle(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "id-1", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning, RepoName: "x"}
	_ = d.store.Put(inst)

	if err := d.CancelRun("id-1"); err != nil {
		t.Errorf("idle cancel should be nil error, got %v", err)
	}
}

func TestCancelRunUnknownInstance(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)

	if err := d.CancelRun("nope"); err == nil {
		t.Error("unknown instance should return error")
	}
}

func TestModelAndThinkingFlagsForwarded(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "ok"}}}
	d, _ := newTestDispatcher(t, r)

	repo := t.TempDir()
	// Write per-repo agent config so the dispatcher picks up model/thinking.
	if err := writeAgentJSON(t, repo, `{"model":"opus","thinking":"high"}`); err != nil {
		t.Fatal(err)
	}

	inst := storage.Instance{InstanceID: "i4", ChatID: 1, TopicID: 1, RepoPath: repo, State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "hi"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	got := r.lastCall()
	if got.Model != "opus" {
		t.Errorf("Model: got %q", got.Model)
	}
	if got.Thinking != "high" {
		t.Errorf("Thinking: got %q", got.Thinking)
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
}

func TestErrorEventSurfacesAsTelegramMessage(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvError, Text: "overloaded_error: rate limit"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "i5", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))

	waitFor(t, 2*time.Second, func() bool { return len(rec.sentTexts()) >= 1 })

	texts := rec.sentTexts()
	if !contains(texts[0], "agent error") {
		t.Errorf("first sent text should mention 'agent error'; got %q", texts[0])
	}
}

// --- helpers ---

func writeAgentJSON(t *testing.T, repoPath, body string) error {
	t.Helper()
	dir := filepath.Join(repoPath, ".trd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "agent.json"), []byte(body), 0o600)
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// waitFor polls cond until true or timeout. Test helper.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %v", timeout)
}
