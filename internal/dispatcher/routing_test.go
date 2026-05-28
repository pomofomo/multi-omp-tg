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

	"github.com/pomofomo/multi-omp-tg/internal/agent"
	"github.com/pomofomo/multi-omp-tg/internal/storage"
	"github.com/pomofomo/multi-omp-tg/internal/telegram"
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
	edits     []editEvent
}

type reaction struct {
	chatID int64
	msgID  int
	emoji  string
}

type editEvent struct {
	chatID  int64
	msgID   int
	text    string
}

func (r *recordingTelegram) sendMessage(_ context.Context, p telegram.SendMessageParams) (telegram.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, p)
	return telegram.Message{MessageID: 9999}, nil
}


func (r *recordingTelegram) editMessage(_ context.Context, p telegram.EditMessageTextParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edits = append(r.edits, editEvent{chatID: p.ChatID, msgID: p.MessageID, text: p.Text})
	return nil
}

func (r *recordingTelegram) sentMessages() []telegram.SendMessageParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]telegram.SendMessageParams, len(r.sent))
	copy(out, r.sent)
	return out
}

func (r *recordingTelegram) editTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.edits))
	for i, e := range r.edits {
		out[i] = e.text
	}
	return out
}

func (r *recordingTelegram) editCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.edits)
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
	d.editMessage = rec.editMessage
	// Voice tool seams. Default to "TTS unavailable, never called" so
	// tests that don't exercise tg_voice behave like a host without TTS.
	// Tests that need to verify voice routing override these directly.
	d.canSynthesize = func() bool { return false }
	d.sendVoice = func(_ context.Context, _ int64, _, _ int, _ string, _ string) (telegram.Message, error) {
		t.Fatalf("sendVoice called unexpectedly")
		return telegram.Message{}, nil
	}
	d.synthesize = func(_ context.Context, _, _ string) (string, error) {
		t.Fatalf("synthesize called unexpectedly")
		return "", nil
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

func TestEyesReactionFiresOnEveryEnqueue(t *testing.T) {
	// Both the immediate-spawn path AND the queued path MUST set 👀, so
	// the sender sees the dispatcher's "got it" mark regardless of whether
	// the agent is already busy. The LLM is expected to layer 👍 on top
	// via tg_react once it has actually read the message.
	r := &fakeRunner{} // first run hangs so we can also test queue path
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "ieyes", ChatID: 7, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 7, thread: 1, msgID: 100, text: "a"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	d.enqueueOrRun(inst, queuedPrompt{chatID: 7, thread: 1, msgID: 101, text: "b"})

	waitFor(t, 2*time.Second, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.reactions) >= 2
	})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reactions) < 2 {
		t.Fatalf("expected ≥2 reactions; got %d", len(rec.reactions))
	}
	seen := map[int]string{}
	for _, r := range rec.reactions {
		seen[r.msgID] = r.emoji
	}
	for _, msgID := range []int{100, 101} {
		if seen[msgID] != "👀" {
			t.Errorf("msg %d should have 👀 from dispatcher; got %q (all=%v)", msgID, seen[msgID], rec.reactions)
		}
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
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

func TestPostFinalErrorDoesNotPolluteReply(t *testing.T) {
	// omp commonly emits a tail-end EPIPE / truncated-JSON EvError when
	// its stdout is torn down after the canonical message_end. The
	// dispatcher must surface the clean final reply unchanged and only
	// log the post-final error for forensics.
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvAssistantFinal, Text: "clean final reply"},
		{Kind: agent.EvError, Text: "agent: bad json line: {\"type\":\"agent_end\","},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "ipostfinal", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))

	waitFor(t, 2*time.Second, func() bool { return len(rec.sentTexts()) >= 1 })

	texts := rec.sentTexts()
	if texts[0] != "clean final reply" {
		t.Errorf("reply must be the canonical final text, untouched; got %q", texts[0])
	}
	if contains(texts[0], "agent reported") || contains(texts[0], "bad json") {
		t.Errorf("post-final EvError must not leak into the reply; got %q", texts[0])
	}
}

func TestErrorBeforeFinalStillSurfaces(t *testing.T) {
	// Regression guard: the suppression must NOT swallow errors that
	// arrived before any final message. Those are real failures and the
	// user needs to see them.
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvError, Text: "overloaded_error: rate limit"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "iprefinal", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))

	waitFor(t, 2*time.Second, func() bool { return len(rec.sentTexts()) >= 1 })
	if !contains(rec.sentTexts()[0], "agent error") {
		t.Errorf("pre-final EvError must still surface; got %q", rec.sentTexts()[0])
	}
}

func TestExtensionWiringPropagatedToAgent(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "ok"}}}
	d, _ := newTestDispatcher(t, r)
	d.extPath = "/fake/ext/tg.ts"
	d.opts.Port = 9999

	inst := storage.Instance{InstanceID: "iext", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 42, thread: 1, msgID: 7, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	got := r.lastCall()
	if len(got.Extensions) != 1 || got.Extensions[0] != "/fake/ext/tg.ts" {
		t.Errorf("Extensions: got %v", got.Extensions)
	}
	if got.AppendSystemPrompt == "" {
		t.Errorf("AppendSystemPrompt should be populated when extension is wired")
	}
	wantEnv := []string{
		"TRD_CHAT_ID=42",
		"TRD_MESSAGE_ID=7",
		"TRD_DISPATCHER_URL=http://127.0.0.1:9999",
	}
	for _, w := range wantEnv {
		found := false
		for _, e := range got.ExtraEnv {
			if e == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ExtraEnv missing %q; got %v", w, got.ExtraEnv)
		}
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
}

func TestExtensionWiringOmittedWhenNoExtension(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "ok"}}}
	d, _ := newTestDispatcher(t, r)
	// extPath deliberately left empty.

	inst := storage.Instance{InstanceID: "inoext", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	got := r.lastCall()
	if len(got.Extensions) != 0 || got.AppendSystemPrompt != "" || len(got.ExtraEnv) != 0 {
		t.Errorf("extension wiring leaked into options when extPath was empty: %+v", got)
	}
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
}

func TestReactToMessageCallsSetReaction(t *testing.T) {
	r := &fakeRunner{}
	d, rec := newTestDispatcher(t, r)

	if err := d.ReactToMessage(1234, 56, "👍"); err != nil {
		t.Fatalf("ReactToMessage: %v", err)
	}
	if len(rec.reactions) != 1 {
		t.Fatalf("expected 1 reaction; got %v", rec.reactions)
	}
	got := rec.reactions[0]
	if got.chatID != 1234 || got.msgID != 56 || got.emoji != "👍" {
		t.Errorf("reaction: %+v", got)
	}
}

func TestReactToMessageRejectsZeroArgs(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)
	for _, tc := range []struct {
		name              string
		chat              int64
		msg               int
		emoji             string
	}{
		{"zero chat", 0, 1, "👍"},
		{"zero msg", 1, 0, "👍"},
		{"empty emoji", 1, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.ReactToMessage(tc.chat, tc.msg, tc.emoji); err == nil {
				t.Errorf("expected error for %s; got nil", tc.name)
			}
		})
	}
}

func TestShutdownCancelsInFlightRuns(t *testing.T) {
	r := &fakeRunner{} // No initial events: the run "hangs" until Cancel.
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "isd", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "x"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	handle := r.pendingAt(0)
	if handle == nil {
		t.Fatal("no handle produced")
	}

	done := make(chan struct{})
	go func() {
		d.Shutdown(time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return within 3s")
	}
	if !handle.cancelled.Load() {
		t.Errorf("handle.Cancel was not invoked during Shutdown")
	}
	// driveAgentRun must have returned; runs map clear.
	d.runMu.Lock()
	defer d.runMu.Unlock()
	if len(d.runs) != 0 {
		t.Errorf("runs map not drained: %d entries", len(d.runs))
	}
}

func TestShutdownDropsQueuedPrompts(t *testing.T) {
	r := &fakeRunner{} // First run hangs.
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "iq", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "first"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })

	// Two more prompts queue up behind the in-flight first run.
	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 2, text: "second"})
	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 3, text: "third"})

	d.runMu.Lock()
	if got := len(d.pendingQueue[inst.InstanceID]); got != 2 {
		d.runMu.Unlock()
		t.Fatalf("expected 2 queued prompts; got %d", got)
	}
	d.runMu.Unlock()

	d.Shutdown(time.Second)

	// After Shutdown, finishRun's queue-drain must NOT have respawned the
	// queued prompts; the runner should still show only the first call.
	if r.callCount() != 1 {
		t.Errorf("queued prompts were respawned during shutdown: callCount=%d", r.callCount())
	}
	d.runMu.Lock()
	defer d.runMu.Unlock()
	if got := len(d.pendingQueue[inst.InstanceID]); got != 0 {
		t.Errorf("pendingQueue not cleared: %d remain", got)
	}
}

func TestShutdownIsNoopWhenIdle(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)

	start := time.Now()
	d.Shutdown(5 * time.Second)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("Shutdown blocked %v with no in-flight work; want near-zero", elapsed)
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

// TestDriveAgentRunStreamsDeltasIncrementally verifies that the
// streaming wiring is engaged when omp emits text deltas. Because the
// production 1.5 s debounce can collapse a synchronous burst of deltas
// into a single Send (the desirable fast-path optimisation), we assert
// the user-visible contract — exactly one message lands, replies to
// the originating message, and ends up with the canonical text — and
// leave the placeholder+edit timing to the stream_test.go unit tests
// which control debounce directly.
func TestDriveAgentRunStreamsDeltasIncrementally(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvSessionID, SessionID: "sess-stream"},
		{Kind: agent.EvAssistantDelta, Text: "Hello"},
		{Kind: agent.EvAssistantDelta, Text: ", "},
		{Kind: agent.EvAssistantDelta, Text: "world"},
		{Kind: agent.EvAssistantFinal, Text: "Hello, world"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{
		InstanceID: "stream-1",
		ChatID:     1, TopicID: 1,
		RepoPath: t.TempDir(),
		State:    storage.StateRunning,
	}
	if err := d.store.Put(inst); err != nil {
		t.Fatal(err)
	}

	d.enqueueOrRun(inst, queuedPrompt{
		chatID: 1, thread: 1, msgID: 555, text: "say hi",
	})

	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))

	waitFor(t, 3*time.Second, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.sent) >= 1
	})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sent) != 1 {
		t.Fatalf("expected exactly 1 send for streaming reply, got %d", len(rec.sent))
	}
	if rec.sent[0].ReplyToMessageID != 555 {
		t.Errorf("placeholder must reply-to original msg 555, got %d",
			rec.sent[0].ReplyToMessageID)
	}
	// Final visible text is the concatenation of streamed deltas
	// (NOT the message_end content). This guards the 2026-05-28 bug
	// where multi-segment turns (text → tool → text) saw their full
	// streamed reply overwritten by just the last segment's
	// message_end content. The streamed buffer is the source of
	// truth — it's what the user has been watching grow.
	visible := rec.sent[0].Text
	if len(rec.edits) > 0 {
		visible = rec.edits[len(rec.edits)-1].text
	}
	if visible != "Hello, world" {
		t.Errorf("final visible text: got %q want %q (streamed buf)", visible, "Hello, world")
	}
}

// TestDriveAgentRunMultiSegmentTurnAccumulates exercises the
// 2026-05-28 regression directly: when omp emits multiple
// EvAssistantFinal events in a single turn (one per text segment
// between tool calls), the streamed deltas — which the user has been
// watching grow — must remain visible at the end. Pre-fix, the last
// segment's message_end overwrote the buffer and Finalize replaced
// the entire reply with just that last segment.
func TestDriveAgentRunMultiSegmentTurnAccumulates(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{
		// Segment 1: "Reading the file..."
		{Kind: agent.EvAssistantDelta, Text: "Reading "},
		{Kind: agent.EvAssistantDelta, Text: "the file..."},
		{Kind: agent.EvAssistantFinal, Text: "Reading the file..."},
		// Simulated tool call between segments. The tool event itself
		// is not consumed by the dispatcher (we just log it), so its
		// presence or absence here doesn't affect the test.
		{Kind: agent.EvToolCall, Text: "read"},
		// Segment 2: "Done!"
		{Kind: agent.EvAssistantDelta, Text: "Done!"},
		{Kind: agent.EvAssistantFinal, Text: "Done!"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{
		InstanceID: "multi-1",
		ChatID:     1, TopicID: 1,
		RepoPath: t.TempDir(),
		State:    storage.StateRunning,
	}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 7, text: "read it"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
	waitFor(t, 3*time.Second, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.sent) >= 1
	})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	visible := rec.sent[0].Text
	if len(rec.edits) > 0 {
		visible = rec.edits[len(rec.edits)-1].text
	}
	const want = "Reading the file...Done!"
	if visible != want {
		t.Errorf("multi-segment final text: got %q want %q (regression: last segment overwrote full reply)",
			visible, want)
	}
}

// TestDriveAgentRunNonStreamingPathUnchanged guards the one-shot path
// that pre-streaming tests already depend on: a turn with only
// EvAssistantFinal (no deltas) goes through sendMessage with no edits.
func TestDriveAgentRunNonStreamingPathUnchanged(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{
		{Kind: agent.EvAssistantFinal, Text: "bare reply"},
	}}
	d, rec := newTestDispatcher(t, r)

	inst := storage.Instance{
		InstanceID: "bare-1",
		ChatID:     1, TopicID: 1,
		RepoPath: t.TempDir(),
		State:    storage.StateRunning,
	}
	_ = d.store.Put(inst)

	d.enqueueOrRun(inst, queuedPrompt{chatID: 1, thread: 1, msgID: 1, text: "hi"})
	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	drainHandle(t, d, inst.InstanceID, r.pendingAt(0))
	waitFor(t, 2*time.Second, func() bool { return len(rec.sentTexts()) == 1 })

	if got := rec.sentTexts()[0]; got != "bare reply" {
		t.Errorf("non-streaming send: got %q", got)
	}
	if got := rec.editCount(); got != 0 {
		t.Errorf("non-streaming path should not edit, got %d edits", got)
	}
}
