package dispatcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pomofomo/multi-omp-tg/internal/telegram"
)

// streamRecorder captures every send and edit emitted by a stream
// under test. Send returns a monotonically-increasing message id so
// tests can verify which message a subsequent edit targeted.
type streamRecorder struct {
	mu        sync.Mutex
	nextID    int32
	sends     []telegram.SendMessageParams
	edits     []telegram.EditMessageTextParams
	sendErr   error
	editErr   error
	editErrOn int  // when >0, only the Nth edit (1-indexed) errors
	editCalls int  // total edit attempts (incl. failed)
}

func newStreamRecorder() *streamRecorder {
	r := &streamRecorder{}
	r.nextID = 1000
	return r
}

func (r *streamRecorder) Send(_ context.Context, p telegram.SendMessageParams) (telegram.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sendErr != nil {
		return telegram.Message{}, r.sendErr
	}
	id := int(atomic.AddInt32(&r.nextID, 1))
	r.sends = append(r.sends, p)
	return telegram.Message{MessageID: id}, nil
}

func (r *streamRecorder) Edit(_ context.Context, p telegram.EditMessageTextParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.editCalls++
	if r.editErr != nil && (r.editErrOn == 0 || r.editErrOn == r.editCalls) {
		return r.editErr
	}
	r.edits = append(r.edits, p)
	return nil
}

func (r *streamRecorder) sendCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sends) }
func (r *streamRecorder) editCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.edits) }

func (r *streamRecorder) editAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.editCalls
}

func (r *streamRecorder) clearEditErr() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.editErr = nil
}
func (r *streamRecorder) lastSend() telegram.SendMessageParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends[len(r.sends)-1]
}

func (r *streamRecorder) lastEdit() telegram.EditMessageTextParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.edits[len(r.edits)-1]
}

// newTestStream returns a streamingReply with the production constants
// overridden for fast unit tests: a 10 ms debounce so Append-then-wait
// fires within a few ms, and (when callers need it) a smaller maxChunk
// set via test-only assignment.
func newTestStream(rec *streamRecorder) *streamingReply {
	s := newStreamingReply(rec.Send, rec.Edit, -1001, 42, 99, discardLogger(), "test")
	s.debounce = 10 * time.Millisecond
	return s
}

func TestStreamingReplyAppendSendsPlaceholderAfterDebounce(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	t.Cleanup(s.Close)

	s.Append("hello")
	// Must not have sent synchronously — debounce is in play.
	if rec.sendCount() != 0 {
		t.Errorf("send fired synchronously: %d", rec.sendCount())
	}
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	got := rec.lastSend()
	if got.Text != "hello" {
		t.Errorf("placeholder text: got %q", got.Text)
	}
	if got.ReplyToMessageID != 99 {
		t.Errorf("placeholder should reply to original msg id 99, got %d", got.ReplyToMessageID)
	}
	if got.ChatID != -1001 || got.MessageThreadID != 42 {
		t.Errorf("addressing: chat=%d thread=%d", got.ChatID, got.MessageThreadID)
	}
}

func TestStreamingReplyDebouncesMultipleAppends(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	s.debounce = 40 * time.Millisecond
	t.Cleanup(s.Close)

	// Five appends each 10 ms apart: total span 40 ms, all under the
	// 40 ms debounce window so only ONE send should fire after the
	// last append plus debounce.
	for _, w := range []string{"He", "llo", ", w", "or", "ld"} {
		s.Append(w)
		time.Sleep(10 * time.Millisecond)
	}
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })
	if got := rec.lastSend().Text; got != "Hello, world" {
		t.Errorf("coalesced text: got %q", got)
	}
	if rec.editCount() != 0 {
		t.Errorf("no edits expected before any rollover/finalize: %d", rec.editCount())
	}
}

func TestStreamingReplyEditsAfterPlaceholder(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	t.Cleanup(s.Close)

	// First batch → placeholder.
	s.Append("first")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })
	placeholderID := rec.lastSend()
	_ = placeholderID

	// Second batch arrives after the placeholder lands → edit.
	s.Append(" second")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.editCount() == 1 })
	if got := rec.lastEdit().Text; got != "first second" {
		t.Errorf("edit text: got %q", got)
	}
}

func TestStreamingReplySkipsIdenticalEdits(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	t.Cleanup(s.Close)

	s.Append("snap")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	// Force a tick without appending more text by manually waiting for
	// the next debounce window — there's nothing to flush, so no edit.
	time.Sleep(50 * time.Millisecond)
	if rec.editCount() != 0 {
		t.Errorf("edit fired with unchanged text: %d", rec.editCount())
	}
}

func TestStreamingReplyFinalizeReplacesText(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)

	s.Append("partial")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	if err := s.Finalize(context.Background(), "canonical reply"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if rec.editCount() != 1 {
		t.Fatalf("expected exactly one edit on Finalize, got %d", rec.editCount())
	}
	if got := rec.lastEdit().Text; got != "canonical reply" {
		t.Errorf("finalize text: got %q", got)
	}

	// Post-Finalize Append must be a no-op.
	s.Append(" oops")
	time.Sleep(50 * time.Millisecond)
	if rec.editCount() != 1 {
		t.Errorf("Append after Finalize triggered another edit: %d", rec.editCount())
	}
}

func TestStreamingReplyFinalizeWithoutDeltasSendsFresh(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)

	// No prior Append — Finalize should open a new message.
	if err := s.Finalize(context.Background(), "bare reply"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if rec.sendCount() != 1 {
		t.Errorf("send count: want 1, got %d", rec.sendCount())
	}
	if rec.editCount() != 0 {
		t.Errorf("edit count: want 0, got %d", rec.editCount())
	}
	if got := rec.lastSend().Text; got != "bare reply" {
		t.Errorf("send text: %q", got)
	}
	if got := rec.lastSend().ReplyToMessageID; got != 99 {
		t.Errorf("bare-finalize should still reply-to original: got %d", got)
	}
}

func TestStreamingReplyRollsOverWhenOverMaxChunk(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	s.maxChunk = 20
	t.Cleanup(s.Close)

	// Phase 1: a small placeholder fits comfortably under maxChunk.
	s.Append("12345")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })
	if got := rec.sends[0].Text; got != "12345" {
		t.Fatalf("placeholder text: got %q", got)
	}

	// Phase 2: appending pushes the buffer to 23 chars. No newline in
	// the first 20, so flushLocked raw-cuts at 20: edit placeholder
	// with the prefix, send a fresh message for the tail "KLM".
	s.Append("67890ABCDEFGHIJKLM")
	waitForCond(t, 250*time.Millisecond, func() bool {
		return rec.sendCount() == 2 && rec.editCount() == 1
	})

	if got := rec.lastEdit().Text; got != "1234567890ABCDEFGHIJ" {
		t.Errorf("freeze-edit text: got %q", got)
	}
	if got := rec.sends[1].Text; got != "KLM" {
		t.Errorf("rollover send text: got %q", got)
	}
	// Rollover messages must NOT claim to be a reply to the user's
	// original message — the first chunk already filled that role.
	if got := rec.sends[1].ReplyToMessageID; got != 0 {
		t.Errorf("rollover send should not claim reply-to, got %d", got)
	}
}

func TestStreamingReplyFinalizeRollsOverIfNeeded(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	s.maxChunk = 10
	t.Cleanup(s.Close)

	// Stream a small placeholder, then finalize with text that's
	// longer than maxChunk. The finalizer should freeze the current
	// message and roll over for the tail.
	s.Append("abc")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	if err := s.Finalize(context.Background(), "abcdefghijKLMNOPQRST"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Edit current with first 10 chars; send fresh for next 10.
	if rec.editCount() < 1 {
		t.Fatalf("expected ≥1 edit, got %d", rec.editCount())
	}
	if rec.sendCount() != 2 {
		t.Fatalf("expected 2 sends after rollover, got %d", rec.sendCount())
	}
	if got := rec.sends[1].Text; got != "KLMNOPQRST" {
		t.Errorf("rollover tail: got %q", got)
	}
}

func TestStreamingReplyClosePreventsFurtherWork(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)

	s.Append("first")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	s.Close()
	s.Append(" more")
	time.Sleep(50 * time.Millisecond)

	if rec.editCount() != 0 {
		t.Errorf("edit fired after Close: %d", rec.editCount())
	}
}

func TestStreamingReplyStreamedReturnsConcatenation(t *testing.T) {
	rec := newStreamRecorder()
	s := newTestStream(rec)
	t.Cleanup(s.Close)

	s.Append("Hello")
	s.Append(", ")
	s.Append("world")
	if got := s.Streamed(); got != "Hello, world" {
		t.Errorf("Streamed: got %q", got)
	}
}

func TestStreamingReplyEditFailureDoesNotAdvanceLastSent(t *testing.T) {
	rec := newStreamRecorder()
	rec.editErr = errors.New("telegram 429")
	s := newTestStream(rec)
	t.Cleanup(s.Close)

	// First flush: send placeholder (no edit yet) — succeeds.
	s.Append("first")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.sendCount() == 1 })

	// Second flush: attempt edit, which errors. lastSent must NOT
	// advance to the new text, so the next flush retries.
	s.Append(" second")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.editAttempts() >= 1 })

	// Disable the error and append more — next flush should edit with
	// the full accumulated text, including the previously-failed bytes.
	rec.clearEditErr()

	s.Append(" third")
	waitForCond(t, 250*time.Millisecond, func() bool { return rec.editCount() == 1 })

	if got := rec.lastEdit().Text; got != "first second third" {
		t.Errorf("retry edit text: got %q", got)
	}
}

func TestSplitAtBoundary(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		max       int
		wantHead  string
		wantTail  string
	}{
		{"under max", "hello", 10, "hello", ""},
		{"exact max", "hello", 5, "hello", ""},
		{"raw cut, no newline", "abcdefghij", 4, "abcd", "efghij"},
		{"newline in window prefers boundary", "abc\ndef", 5, "abc", "def"},
		{"newline at very start kept verbatim", "\nabcdef", 4, "\nabc", "def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, tl := splitAtBoundary(tc.in, tc.max)
			if h != tc.wantHead || tl != tc.wantTail {
				t.Errorf("got (%q, %q), want (%q, %q)", h, tl, tc.wantHead, tc.wantTail)
			}
		})
	}
}

func TestRemainderAfter(t *testing.T) {
	cases := []struct {
		text string
		n    int
		want string
	}{
		{"hello world", 6, "world"},
		{"hello", 5, ""},
		{"hello", 10, ""},
		{"hello", 0, "hello"},
		{"hello", -3, "hello"},
	}
	for _, tc := range cases {
		if got := remainderAfter(tc.text, tc.n); got != tc.want {
			t.Errorf("remainderAfter(%q, %d) = %q, want %q", tc.text, tc.n, got, tc.want)
		}
	}
}

// waitForCond polls until cond returns true or timeout fires. Test
// helper distinct from waitFor (in routing_test.go) so failure messages
// mention "stream" context — easier to debug.
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForCond: condition not met within %v", timeout)
}
