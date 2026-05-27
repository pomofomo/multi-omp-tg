// Package agent wraps `omp -p --mode json` as a one-shot subprocess that
// emits classified events over a Go channel. The dispatcher consumes these
// events to ferry assistant text back to Telegram and to capture omp's
// session id so subsequent prompts can be resumed with `--resume`.
//
// Wire format (recorded in porting/omp-sample.ndjson, verbatim 14-line
// capture from omp v15.3.0):
//
//   line 1: {"type":"session","version":3,"id":"<UUID>","cwd":"...","timestamp":"..."}
//   line 2: {"type":"agent_start"}
//   line 3: {"type":"turn_start"}
//   line 4: {"type":"message_start","message":{"role":"user", ...}}
//   line 5: {"type":"message_end",  "message":{"role":"user", ...}}
//   line 6: {"type":"message_start","message":{"role":"assistant", ...}}
//   line 7..N: {"type":"message_update","assistantMessageEvent":{"type":"text_start"|"text_delta"|"text_end", ...}}
//   line N+1: {"type":"message_end",  "message":{"role":"assistant","content":[{"type":"text","text":"…"}],"stopReason":"stop"|"error","errorMessage":"…"}}
//   line N+2: {"type":"turn_end","message":{...},"toolResults":[]}
//   line last: {"type":"agent_end","messages":[...]}
//
// We deliberately decode only the small slices of each line we need, both
// for clarity and to avoid allocating the huge `usage`/`partial` blobs on
// every streaming chunk.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Kind classifies an Event for switch-case consumers.
type Kind int

const (
	// EvSessionID is emitted once per run, on the first line. Text is unused;
	// SessionID carries the omp session UUID.
	EvSessionID Kind = iota
	// EvAssistantDelta is one incremental piece of assistant text from a
	// `text_delta` stream event. Text is the cumulative-safe delta string.
	EvAssistantDelta
	// EvAssistantFinal is the finalized assistant message text emitted when
	// `message_end` arrives for role=assistant without a stop=error. Text is
	// the concatenation of every text content part.
	EvAssistantFinal
	// EvError surfaces any non-recoverable error (API overloaded, malformed
	// JSON, omp process failure). Text is a human-readable message.
	EvError
	// EvDone is emitted exactly once when the omp process exits cleanly.
	// Consumers MUST treat this as the final event in the stream — the
	// channel will be closed immediately after.
	EvDone
)

// String returns a human label for the kind (used in logs and tests).
func (k Kind) String() string {
	switch k {
	case EvSessionID:
		return "session_id"
	case EvAssistantDelta:
		return "assistant_delta"
	case EvAssistantFinal:
		return "assistant_final"
	case EvError:
		return "error"
	case EvDone:
		return "done"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Event is one classified item in the run stream.
type Event struct {
	Kind      Kind
	SessionID string          // EvSessionID
	Text      string          // EvAssistantDelta, EvAssistantFinal, EvError
	Raw       json.RawMessage // the original NDJSON line, for diagnostics; nil on EvDone
}

// RunOptions controls how `omp -p` is invoked. Zero-value defaults are
// safe; only Cwd and Prompt are required to produce a useful run.
type RunOptions struct {
	// Cwd is the working directory for the omp process. omp scopes its
	// session storage by cwd, so this MUST be the cloned repo path.
	Cwd string
	// SessionID, if non-empty, is passed as `--resume <SessionID>`. omp
	// resolves it within the cwd's session directory; on a miss the run
	// fails with a non-zero exit and an error message on stderr.
	SessionID string
	// Model is forwarded as `omp --model <Model>`. Empty omits the flag.
	Model string
	// Thinking is forwarded as `omp --thinking <Thinking>`. Empty omits.
	// Accepted by omp: minimal, low, medium, high, xhigh.
	Thinking string
	// Prompt is the user's message text, passed as the final positional
	// argument to omp (omp -p reads from argv, not stdin).
	Prompt string
	// LogPath is the file to append omp's stderr to. Empty discards stderr.
	LogPath string
	// Binary overrides the omp executable path. Empty falls back to
	// $TRD_OMP_BIN then "omp" on PATH.
	Binary string
	// Extensions are forwarded as repeated `--extension <path>` flags.
	// Each path MUST point to a TypeScript file omp can load via jiti.
	// The TRD dispatcher uses this to inject its Telegram-aware tools
	// (see internal/agent/extension).
	Extensions []string
	// AppendSystemPrompt is forwarded as `--append-system-prompt <value>`.
	// omp resolves a single-line value as a file path before falling
	// back to literal text — keep a newline in the value when you want
	// the literal-text path.
	AppendSystemPrompt string
	// ExtraEnv are extra `KEY=VALUE` entries appended on top of the
	// parent process environment for the spawned omp. The dispatcher
	// uses this to surface TRD_CHAT_ID / TRD_MESSAGE_ID /
	// TRD_DISPATCHER_URL to the tg_react tool.
	ExtraEnv []string
	// ExtraArgs are appended verbatim to the argv (between the flags and
	// the prompt). Reserved for advanced/experimental tests; production
	// callers leave it nil.
	ExtraArgs []string
}

// Run handle for an in-flight (or completed) omp invocation. The Events
// channel is closed exactly once, after the process exits.
type Run struct {
	cmd    *exec.Cmd
	events chan Event

	exitMu  sync.Mutex
	exitErr error // nil until Wait returns
	exited  bool
}

// Events returns the receive-only event stream. Always range over it —
// the channel closes when the process exits.
func (r *Run) Events() <-chan Event { return r.events }

// Cmd exposes the underlying exec.Cmd for direct signal delivery
// (e.g. `cmd.Process.Signal(os.Interrupt)`). Returns nil if the run is
// not yet started.
func (r *Run) Cmd() *exec.Cmd { return r.cmd }

// Wait blocks until the omp process exits and returns its exit status.
// Safe to call multiple times — subsequent calls return the cached error.
// Returns nil on a clean (exit 0) run.
func (r *Run) Wait() error {
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	if r.exited {
		return r.exitErr
	}
	r.exitErr = r.cmd.Wait()
	r.exited = true
	return r.exitErr
}

// Cancel signals the omp process group with SIGINT, waits up to grace for
// a clean exit, then SIGKILLs the group. Safe to call from any goroutine
// and idempotent across multiple invocations.
func (r *Run) Cancel(grace time.Duration) {
	if r.cmd == nil || r.cmd.Process == nil {
		return
	}
	// Signal the whole process group (Setpgid:true on Start). Negative pid
	// in syscall.Kill addresses the group leader's group.
	pgid, err := syscall.Getpgid(r.cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		// Fallback to signalling the single process.
		_ = r.cmd.Process.Signal(syscall.SIGINT)
	} else {
		_ = syscall.Kill(-pgid, syscall.SIGINT)
	}
	if grace <= 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = r.cmd.Process.Kill()
		}
	}
}

// Start spawns omp -p with the requested options and returns immediately.
// A background goroutine reads stdout, parses NDJSON, emits events on the
// returned Run's Events channel, and closes the channel when the process
// exits. On a spawn-time error (binary missing, exec failure) Start
// returns a non-nil error and Run is nil.
//
// Cancellation: pass a context whose cancellation should terminate the
// run. The context is wired into exec.CommandContext, which sends SIGKILL
// to the process when Done. For graceful shutdown, prefer Run.Cancel.
func Start(ctx context.Context, opts RunOptions) (*Run, error) {
	if opts.Cwd == "" {
		return nil, errors.New("agent.Start: Cwd is required")
	}
	if opts.Prompt == "" {
		return nil, errors.New("agent.Start: Prompt is required")
	}

	args := []string{"-p", "--mode", "json"}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Thinking != "" {
		args = append(args, "--thinking", opts.Thinking)
	}
	for _, ext := range opts.Extensions {
		if ext == "" {
			continue
		}
		args = append(args, "--extension", ext)
	}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if len(opts.ExtraArgs) > 0 {
		args = append(args, opts.ExtraArgs...)
	}
	args = append(args, opts.Prompt)

	bin := opts.Binary
	if bin == "" {
		bin = os.Getenv("TRD_OMP_BIN")
	}
	if bin == "" {
		bin = "omp"
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(opts.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), opts.ExtraEnv...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent: stdout pipe: %w", err)
	}

	var logFile *os.File
	if opts.LogPath != "" {
		logFile, err = os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			_ = stdout.Close()
			return nil, fmt.Errorf("agent: open log %s: %w", opts.LogPath, err)
		}
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("agent: start %s: %w", bin, err)
	}

	r := &Run{
		cmd:    cmd,
		events: make(chan Event, 32),
	}
	go parseLoop(stdout, r.events, logFile, r)
	return r, nil
}

// parseLoop is the single reader goroutine. It scans NDJSON lines, emits
// one classified event per line (or none, for swallowed types), then
// drains the process exit and emits EvDone before closing the channel.
//
// The scanner buffer is sized to 16 MB because omp's `message_update`
// lines include the entire cumulative partial message and can easily
// exceed the default 64 KB once a few tool calls have run.
func parseLoop(stdout io.ReadCloser, ch chan<- Event, logFile *os.File, r *Run) {
	defer close(ch)
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, ok := classify(line)
		if !ok {
			continue
		}
		ch <- ev
	}
	if err := sc.Err(); err != nil {
		ch <- Event{Kind: EvError, Text: "agent: stdout scan: " + err.Error()}
	}

	exitErr := r.Wait()
	if exitErr != nil {
		ch <- Event{Kind: EvError, Text: "agent: omp exited: " + exitErr.Error()}
	}
	ch <- Event{Kind: EvDone}
}

// classify maps one NDJSON line to (Event, true) or (zero, false) to skip.
// We do not return errors for malformed JSON — those become EvError events
// so consumers can decide whether to surface or suppress them.
//
// Decoding strategy: cheap top-level decode to discover `type`, then a
// second decode into the narrow per-type struct. This avoids allocating
// the giant nested `usage`/`partial` payloads on every streaming chunk.
func classify(line []byte) (Event, bool) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Event{
			Kind: EvError,
			Text: "agent: bad json line: " + truncForLog(line, 200),
			Raw:  cloneRaw(line),
		}, true
	}

	switch head.Type {

	case "session":
		var s struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &s); err != nil || s.ID == "" {
			return Event{}, false
		}
		return Event{Kind: EvSessionID, SessionID: s.ID, Raw: cloneRaw(line)}, true

	case "message_update":
		var u struct {
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal(line, &u); err != nil {
			return Event{}, false
		}
		if u.AssistantMessageEvent.Type != "text_delta" {
			return Event{}, false
		}
		if u.AssistantMessageEvent.Delta == "" {
			return Event{}, false
		}
		return Event{
			Kind: EvAssistantDelta,
			Text: u.AssistantMessageEvent.Delta,
			Raw:  cloneRaw(line),
		}, true

	case "message_end":
		var m struct {
			Message struct {
				Role         string `json:"role"`
				StopReason   string `json:"stopReason"`
				ErrorMessage string `json:"errorMessage"`
				Content      []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return Event{}, false
		}
		if m.Message.Role != "assistant" {
			return Event{}, false
		}
		if m.Message.StopReason == "error" {
			return Event{
				Kind: EvError,
				Text: extractErrorMessage(m.Message.ErrorMessage),
				Raw:  cloneRaw(line),
			}, true
		}
		var b strings.Builder
		for _, c := range m.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		if b.Len() == 0 {
			return Event{}, false
		}
		return Event{
			Kind: EvAssistantFinal,
			Text: b.String(),
			Raw:  cloneRaw(line),
		}, true

	default:
		// agent_start, turn_start, turn_end, agent_end, message_start,
		// model_change, thinking_level_change, auto_retry_start, …
		return Event{}, false
	}
}

// extractErrorMessage tries to pull a human message out of omp's
// errorMessage field, which is sometimes a JSON-encoded provider error.
// On any failure it returns the raw value unchanged.
func extractErrorMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "agent error"
	}
	if !strings.HasPrefix(raw, "{") {
		return raw
	}
	var nested struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &nested); err != nil {
		return raw
	}
	if nested.Error.Message != "" {
		if nested.Error.Type != "" {
			return nested.Error.Type + ": " + nested.Error.Message
		}
		return nested.Error.Message
	}
	return raw
}

// cloneRaw makes a defensive copy of a scanner buffer slice so the Event
// can outlive the next Scan() call.
func cloneRaw(line []byte) json.RawMessage {
	cp := make([]byte, len(line))
	copy(cp, line)
	return cp
}

// truncForLog returns up to max bytes of s, with an ellipsis if truncated.
func truncForLog(s []byte, max int) string {
	if len(s) <= max {
		return string(s)
	}
	return string(s[:max]) + "…"
}
