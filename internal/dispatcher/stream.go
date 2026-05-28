package dispatcher

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pomofomo/multi-omp-tg/internal/telegram"
)

// Telegram constraints we have to live within:
//
//   • sendMessage / editMessageText payloads must be ≤ 4096 chars; we use
//     4000 to leave headroom for the bot's own formatting and accidental
//     overruns from omp's multi-byte runes.
//   • bots may edit a given chat at roughly 1 message/sec without hitting
//     HTTP 429. A 1.5 s debounce window keeps us comfortably under that
//     ceiling while still feeling responsive.
//   • An editMessageText with text identical to what's already displayed
//     returns 400 "message is not modified" — cheap to dedupe locally.
const (
	streamMaxChunk     = 4000
	streamDebounce     = 1500 * time.Millisecond
	streamTickTimeout  = 30 * time.Second
	streamFinalTimeout = 30 * time.Second
)

// streamingReply owns the live-edit state for a single omp turn. The
// dispatcher constructs one when the first EvAssistantDelta arrives,
// pumps deltas through Append, and finishes with Finalize (which forces
// one last edit using omp's canonical message_end text).
//
// State machine, in order:
//
//   1. Append accumulates the delta into `buf` and (re)arms the debounce
//      timer. The first arming sends a placeholder message and captures
//      currentID; subsequent armings edit currentID.
//   2. When buf crosses maxChunk, the current message is "frozen" (one
//      last edit with the prefix), a new placeholder is sent for the
//      tail, and buf restarts from the tail. committedBytes tracks how
//      much text now lives in previously-frozen messages.
//   3. Finalize cancels the debouncer and replaces buf with the suffix of
//      the canonical text that has NOT yet been committed to a frozen
//      message. The same chunking logic applies, so an over-long final
//      text rolls over naturally.
//   4. Close cancels the debouncer without flushing. Used on shutdown.
//
// All mutation is serialised by `mu`. The debounce timer callback grabs
// the same lock, so an Append racing with a tick simply means the tick
// blocks until Append releases and then operates on the latest buf.
//
// The struct is intentionally untied to *Dispatcher: it takes the
// send/edit functions directly. This keeps unit tests trivial.
type streamingReply struct {
	send func(ctx context.Context, p telegram.SendMessageParams) (telegram.Message, error)
	edit func(ctx context.Context, p telegram.EditMessageTextParams) error

	chatID         int64
	threadID       int
	initialReplyTo int // consumed on the first send to bind the reply
	logger         *slog.Logger
	instanceLog    string // shortID, for log records only

	// Tunables — overridable in tests so we don't sleep 1.5 s per case.
	maxChunk int
	debounce time.Duration

	mu             sync.Mutex
	currentID      int             // 0 = no message yet for this chunk
	buf            strings.Builder // text destined for the current message
	total          strings.Builder // every byte we ever Appended (fallback)
	lastSent       string          // last text actually pushed to currentID
	committedBytes int             // bytes in previously-frozen messages
	timer          *time.Timer
	closed         bool
}

// newStreamingReply is a thin constructor; the wiring of send/edit is
// the only thing the dispatcher needs to inject. Defaults to the
// production maxChunk/debounce.
func newStreamingReply(
	send func(ctx context.Context, p telegram.SendMessageParams) (telegram.Message, error),
	edit func(ctx context.Context, p telegram.EditMessageTextParams) error,
	chatID int64, threadID, replyTo int,
	logger *slog.Logger, instanceLog string,
) *streamingReply {
	return &streamingReply{
		send:           send,
		edit:           edit,
		chatID:         chatID,
		threadID:       threadID,
		initialReplyTo: replyTo,
		logger:         logger,
		instanceLog:    instanceLog,
		maxChunk:       streamMaxChunk,
		debounce:       streamDebounce,
	}
}

// Append accumulates a delta and (re)arms the debounce timer so a flush
// runs `debounce` after the last append. Safe to call from the event
// goroutine; no network I/O happens here.
//
// A no-op after Finalize / Close so a late delta from omp's stdout
// teardown can't trigger another edit.
func (s *streamingReply) Append(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.buf.WriteString(delta)
	s.total.WriteString(delta)
	if s.timer == nil {
		s.timer = time.AfterFunc(s.debounce, s.tick)
		return
	}
	s.timer.Reset(s.debounce)
}

// Streamed returns the concatenation of every delta we've seen. The
// dispatcher uses it as the fallback "reply text" when omp dies without
// emitting message_end (no canonical text exists).
func (s *streamingReply) Streamed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total.String()
}

// HasMessage reports whether at least one Telegram message has been
// sent for this stream. Used by callers that want to decide whether to
// fall back to a one-shot send.
func (s *streamingReply) HasMessage() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentID != 0 || s.committedBytes > 0
}

// Finalize cancels the debouncer and forces one last edit so the user
// sees `text` as the complete reply across all messages. text is the
// canonical content (typically EvAssistantFinal.Text, optionally with
// the dispatcher's error annotation appended).
//
// When text is longer than what already fits in the current message,
// the suffix rolls over into one or more fresh messages — same chunking
// as during streaming.
func (s *streamingReply) Finalize(ctx context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	// Compute what should appear in the CURRENT message: the suffix of
	// `text` that hasn't been written to a frozen message yet. If the
	// canonical text turned out shorter than what we streamed
	// (uncommon — omp does it on retries sometimes), tail is empty and
	// we just leave the current message as-is.
	tail := remainderAfter(text, s.committedBytes)
	s.buf.Reset()
	s.buf.WriteString(tail)
	err := s.flushLocked(ctx, true)
	s.closed = true
	return err
}

// Close cancels any pending debounced edit without flushing. Idempotent.
func (s *streamingReply) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
}

// tick is the debounce-timer callback. It re-acquires the lock, checks
// the closed flag (Finalize/Close may have fired after the timer
// scheduled this callback), and pushes a non-forced flush.
func (s *streamingReply) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), streamTickTimeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if err := s.flushLocked(ctx, false); err != nil {
		s.logger.Debug("stream: tick flush errored",
			"instance", s.instanceLog, "err", err)
	}
}

// flushLocked pushes whatever's in buf to Telegram, splitting and
// rolling over as needed. Caller MUST hold s.mu. When force is true the
// edit fires even if the text is unchanged from lastSent (used on
// Finalize to make sure the canonical text wins over the last delta).
func (s *streamingReply) flushLocked(ctx context.Context, force bool) error {
	for {
		text := s.buf.String()
		if text == "" {
			return nil
		}
		if !force && text == s.lastSent {
			return nil
		}
		if len(text) <= s.maxChunk {
			return s.editOrSendLocked(ctx, text)
		}

		// Buffer overflows the current message — freeze: write the
		// largest prefix that fits into currentID (final form for
		// that message), then roll over to a fresh placeholder.
		head, tail := splitAtBoundary(text, s.maxChunk)
		if err := s.editOrSendLocked(ctx, head); err != nil {
			return err
		}
		s.committedBytes += len(head)
		s.currentID = 0
		s.lastSent = ""
		s.buf.Reset()
		s.buf.WriteString(tail)
		// `force` does not survive a rollover: the fresh message
		// will Send (not Edit), and we only need force-edit semantics
		// when there's an existing message whose text might equal
		// what we want to write. Loop iterates to flush the tail.
		force = false
	}
}

// editOrSendLocked routes one chunk to either editMessageText (current
// message exists) or sendMessage (no message yet — opens a new one).
// Caller holds s.mu. Updates currentID / lastSent on success.
func (s *streamingReply) editOrSendLocked(ctx context.Context, text string) error {
	if s.currentID == 0 {
		replyTo := s.initialReplyTo
		// Reply binding is consumed: rollover messages 2..N stay in
		// the topic but don't claim to be a reply to the user's
		// original message (that would be misleading after we've
		// already sent one reply).
		s.initialReplyTo = 0
		m, err := s.send(ctx, telegram.SendMessageParams{
			ChatID:           s.chatID,
			MessageThreadID:  s.threadID,
			Text:             text,
			ReplyToMessageID: replyTo,
		})
		if err != nil {
			s.logger.Warn("stream: send placeholder failed",
				"instance", s.instanceLog, "err", err)
			return err
		}
		s.currentID = m.MessageID
		s.lastSent = text
		return nil
	}
	err := s.edit(ctx, telegram.EditMessageTextParams{
		ChatID:    s.chatID,
		MessageID: s.currentID,
		Text:      text,
	})
	if err != nil {
		// Common benign cases: "message is not modified" (we already
		// deduped, but Telegram occasionally normalises whitespace
		// differently), and HTTP 429 (debounce will retry on the next
		// tick or on Finalize). Don't crash the stream.
		s.logger.Debug("stream: edit failed",
			"instance", s.instanceLog, "msg_id", s.currentID, "err", err)
		return err
	}
	s.lastSent = text
	return nil
}

// splitAtBoundary returns (head, tail) where head is the largest prefix
// of s no longer than max bytes, preferring to cut at the last newline
// in that range. If no newline exists in the prefix, falls back to a
// raw cut at max. The cut newline is dropped (head doesn't include it,
// tail doesn't either) so the user sees a clean break between messages.
func splitAtBoundary(s string, max int) (head, tail string) {
	if len(s) <= max {
		return s, ""
	}
	if i := strings.LastIndex(s[:max], "\n"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s[:max], s[max:]
}

// remainderAfter returns text[n:] when n is in range, or "" when n is
// at-or-past the end (the streamed text overran the canonical). When n
// is negative (shouldn't happen, but be defensive) returns the whole
// string.
func remainderAfter(text string, n int) string {
	if n <= 0 {
		return text
	}
	if n >= len(text) {
		return ""
	}
	return text[n:]
}
