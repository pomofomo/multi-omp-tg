package dispatcher

import (
	"testing"
	"time"

	"github.com/pomofomo/multi-omp-tg/internal/agent"
	"github.com/pomofomo/multi-omp-tg/internal/api"
	"github.com/pomofomo/multi-omp-tg/internal/storage"
)

// TestRequestRestartRejectsNonController guards Proposal C in DEBUG.md:
// only an instance flagged as the controller may trigger a self-restart.
// All other ids — including unknown ids — must surface api.ErrUnauthorized.
func TestRequestRestartRejectsNonController(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeRunner{})

	plain := storage.Instance{InstanceID: "plain", ChatID: 1, TopicID: 1, State: storage.StateRunning}
	if err := d.store.Put(plain); err != nil {
		t.Fatal(err)
	}

	if err := d.RequestRestart(""); err != api.ErrUnauthorized {
		t.Errorf("empty caller: want ErrUnauthorized, got %v", err)
	}
	if err := d.RequestRestart("unknown-id"); err != api.ErrUnauthorized {
		t.Errorf("unknown caller: want ErrUnauthorized, got %v", err)
	}
	if err := d.RequestRestart("plain"); err != api.ErrUnauthorized {
		t.Errorf("non-controller caller: want ErrUnauthorized, got %v", err)
	}
	if d.PendingRestart() {
		t.Error("pendingRestart should remain false after rejected calls")
	}
}

// TestRequestRestartAcceptsControllerAndStopsWhenIdle: a controller-flagged
// caller flips pendingRestart and, with no in-flight runs, immediately
// fires stopForRestart so the outer Run() can unwind for syscall.Exec.
func TestRequestRestartAcceptsControllerAndStopsWhenIdle(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeRunner{})

	ctrl := storage.Instance{InstanceID: "ctrl", ChatID: 1, TopicID: 1, State: storage.StateRunning, Controller: true}
	if err := d.store.Put(ctrl); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})
	d.stopForRestart = func() { close(stopped) }

	if err := d.RequestRestart("ctrl"); err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}
	if !d.PendingRestart() {
		t.Error("pendingRestart should be true after a controller call")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stopForRestart was not invoked when idle")
	}

	// Idempotent: second call is still authorised but does not re-fire
	// stopForRestart (restartOnce guards the call).
	if err := d.RequestRestart("ctrl"); err != nil {
		t.Errorf("second RequestRestart: %v", err)
	}
}

// TestRequestRestartFlushesPendingQueueToBolt covers the lossless
// hand-off: pendingQueue contents must land in deferred_prompts so the
// successor process picks them up.
func TestRequestRestartFlushesPendingQueueToBolt(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeRunner{})

	ctrl := storage.Instance{InstanceID: "ctrl", ChatID: 1, TopicID: 1, State: storage.StateRunning, Controller: true}
	if err := d.store.Put(ctrl); err != nil {
		t.Fatal(err)
	}

	// Pretend a run is in flight so triggerRestartStop is NOT called
	// (otherwise stopForRestart is nil and panics). The point of this
	// test is just to assert the queue flush, not the stop sequence.
	d.runMu.Lock()
	d.runs["ctrl"] = &agentRun{instanceID: "ctrl"}
	d.pendingQueue["ctrl"] = []queuedPrompt{
		{chatID: 1, thread: 2, msgID: 10, user: "alice", text: "first"},
		{chatID: 1, thread: 2, msgID: 11, user: "alice", text: "second"},
	}
	d.runMu.Unlock()

	if err := d.RequestRestart("ctrl"); err != nil {
		t.Fatalf("RequestRestart: %v", err)
	}

	got, err := d.store.DrainDeferred()
	if err != nil {
		t.Fatalf("DrainDeferred: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("deferred count: want 2, got %d (%+v)", len(got), got)
	}
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("FIFO ordering not preserved: %+v", got)
	}

	// pendingQueue must be cleared after flush.
	d.runMu.Lock()
	defer d.runMu.Unlock()
	if got := len(d.pendingQueue["ctrl"]); got != 0 {
		t.Errorf("pendingQueue not cleared: %d remain", got)
	}
}

// TestEnqueueOrRunDefersPromptsDuringRestart: while pendingRestart is
// set, new prompts must skip the run spawn and persist to bbolt instead.
// The fakeRunner must NOT receive any Start call.
func TestEnqueueOrRunDefersPromptsDuringRestart(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)

	ctrl := storage.Instance{InstanceID: "ctrl", ChatID: 1, TopicID: 1, State: storage.StateRunning, Controller: true, RepoPath: t.TempDir()}
	if err := d.store.Put(ctrl); err != nil {
		t.Fatal(err)
	}
	d.pendingRestart.Store(true)

	d.enqueueOrRun(ctrl, queuedPrompt{chatID: 1, thread: 2, msgID: 42, user: "alice", text: "deferred work"})

	// No agent run must spawn. Give the goroutines a moment to settle
	// in case the fake racer fires anyway.
	time.Sleep(50 * time.Millisecond)
	if r.callCount() != 0 {
		t.Fatalf("agent.Start was called %d times during pendingRestart", r.callCount())
	}

	got, err := d.store.DrainDeferred()
	if err != nil {
		t.Fatalf("DrainDeferred: %v", err)
	}
	if len(got) != 1 || got[0].Text != "deferred work" || got[0].MsgID != 42 {
		t.Fatalf("deferred prompt: %+v", got)
	}
}

// TestFinishRunTriggersStopWhenDrainComplete: simulate the natural
// progression — pendingRestart set with one run in flight, finishRun
// removes it, the runs map empties, and stopForRestart fires exactly once.
func TestFinishRunTriggersStopWhenDrainComplete(t *testing.T) {
	d, _ := newTestDispatcher(t, &fakeRunner{})

	stopped := make(chan struct{})
	d.stopForRestart = func() { close(stopped) }

	d.pendingRestart.Store(true)
	d.runMu.Lock()
	d.runs["ctrl"] = &agentRun{instanceID: "ctrl"}
	d.runMu.Unlock()

	// Mimic the run finishing.
	d.finishRun("ctrl")

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stopForRestart was not called after final run drained")
	}
}

// TestRedeliverDeferredPromptsRoutesThroughEnqueueOrRun: a deferred
// prompt left over from a predecessor's restart must produce an actual
// agent run on startup.
func TestRedeliverDeferredPromptsRoutesThroughEnqueueOrRun(t *testing.T) {
	r := &fakeRunner{emit: []agent.Event{{Kind: agent.EvAssistantFinal, Text: "redelivered ok"}}}
	d, _ := newTestDispatcher(t, r)

	inst := storage.Instance{InstanceID: "inst", ChatID: 1, TopicID: 1, RepoPath: t.TempDir(), State: storage.StateRunning}
	if err := d.store.Put(inst); err != nil {
		t.Fatal(err)
	}
	if err := d.store.EnqueueDeferred(storage.DeferredPrompt{
		InstanceID: "inst",
		ChatID:     1,
		ThreadID:   1,
		MsgID:      77,
		Text:       "previously deferred",
	}); err != nil {
		t.Fatal(err)
	}

	d.redeliverDeferredPrompts(nil)

	waitFor(t, 2*time.Second, func() bool { return r.callCount() == 1 })
	if got := r.lastCall().Prompt; got != "previously deferred" {
		t.Errorf("redelivered prompt: got %q", got)
	}

	// Bucket must be empty now.
	again, _ := d.store.DrainDeferred()
	if len(again) != 0 {
		t.Errorf("DrainDeferred after redeliver: want 0, got %d", len(again))
	}

	drainHandle(t, d, "inst", r.pendingAt(0))
}

// TestRedeliverDropsPromptsForVanishedOrStoppedInstances: deferred items
// for instances that have been forgotten (Get returns nil) or /stopped
// in the interim must be dropped, not redelivered.
func TestRedeliverDropsPromptsForVanishedOrStoppedInstances(t *testing.T) {
	r := &fakeRunner{}
	d, _ := newTestDispatcher(t, r)

	stopped := storage.Instance{InstanceID: "stopped", ChatID: 1, TopicID: 1, State: storage.StateStopped, RepoPath: t.TempDir()}
	if err := d.store.Put(stopped); err != nil {
		t.Fatal(err)
	}

	_ = d.store.EnqueueDeferred(storage.DeferredPrompt{InstanceID: "ghost", MsgID: 1, Text: "no-instance"})
	_ = d.store.EnqueueDeferred(storage.DeferredPrompt{InstanceID: "stopped", MsgID: 2, Text: "instance-stopped"})

	d.redeliverDeferredPrompts(nil)
	time.Sleep(50 * time.Millisecond)
	if r.callCount() != 0 {
		t.Fatalf("dropped prompts should not spawn agents, got %d Start calls", r.callCount())
	}
}
