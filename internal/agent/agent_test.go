package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file drive Start() against a fake omp binary. The
// fake is the test binary itself, re-invoked by Start() through exec; the
// child sees TRD_AGENT_TEST_FAKE_OMP_MODE set by t.Setenv() in the parent
// test, and TestMain reroutes those processes into one of the fakeOMP*
// helpers below. The parent process never has the env var set, so it
// falls through to m.Run().
//
// Modes:
//   happy     — full transcript: session, two text_delta, message_end, agent_end
//   error     — session + assistant message with stopReason=error
//   slow      — session, then sleep until SIGINT/timeout
//   bad-json  — one malformed line, then a valid session
//   empty     — exit immediately with no stdout (tests EvDone race)
//   exit-fail — emit a session, then exit non-zero
//
// Sleep duration for `slow` is controlled by TRD_AGENT_TEST_FAKE_OMP_SLEEP_MS.

const (
	helperEnv      = "TRD_AGENT_TEST_FAKE_OMP_MODE"
	helperSleepEnv = "TRD_AGENT_TEST_FAKE_OMP_SLEEP_MS"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "happy":
		fakeOMPHappy()
	case "error":
		fakeOMPError()
	case "slow":
		fakeOMPSlow()
	case "bad-json":
		fakeOMPBadJSON()
	case "empty":
		os.Exit(0)
	case "exit-fail":
		emit(map[string]any{"type": "session", "id": "exit-fail"})
		os.Exit(7)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake omp mode:", os.Getenv(helperEnv))
		os.Exit(2)
	}
}

func fakeOMPHappy() {
	emit(map[string]any{
		"type":    "session",
		"version": 3,
		"id":      "test-session-uuid",
		"cwd":     mustWD(),
	})
	emit(map[string]any{"type": "agent_start"})
	emit(map[string]any{"type": "turn_start"})
	emit(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "Hello ",
		},
	})
	emit(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "world",
		},
	})
	emit(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []map[string]any{
				{"type": "text", "text": "Hello world"},
			},
		},
	})
	emit(map[string]any{"type": "agent_end"})
	os.Exit(0)
}

func fakeOMPError() {
	emit(map[string]any{"type": "session", "id": "err-session"})
	emit(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":         "assistant",
			"stopReason":   "error",
			"errorMessage": `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"content":      []map[string]any{},
		},
	})
	os.Exit(0)
}

func fakeOMPSlow() {
	emit(map[string]any{"type": "session", "id": "slow-session"})
	_ = os.Stdout.Sync()

	dur := 5 * time.Second
	if ms := os.Getenv(helperSleepEnv); ms != "" {
		var n int
		_, _ = fmt.Sscanf(ms, "%d", &n)
		dur = time.Duration(n) * time.Millisecond
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-c:
		os.Exit(130)
	case <-time.After(dur):
		os.Exit(0)
	}
}

func fakeOMPBadJSON() {
	fmt.Println("{not really json")
	emit(map[string]any{"type": "session", "id": "after-bad"})
	emit(map[string]any{"type": "agent_end"})
	os.Exit(0)
}

func emit(obj map[string]any) {
	data, err := json.Marshal(obj)
	if err != nil {
		fmt.Fprintln(os.Stderr, "emit:", err)
		os.Exit(2)
	}
	fmt.Println(string(data))
}

func mustWD() string {
	w, err := os.Getwd()
	if err != nil {
		return ""
	}
	return w
}

// helperBinary returns the path to the running test binary, which doubles
// as the fake omp.
func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// --- classify unit tests ---

func TestClassifySession(t *testing.T) {
	line := []byte(`{"type":"session","id":"abc-123","cwd":"/x"}`)
	ev, ok := classify(line)
	if !ok || ev.Kind != EvSessionID || ev.SessionID != "abc-123" {
		t.Fatalf("classify session: got kind=%s id=%q ok=%v", ev.Kind, ev.SessionID, ok)
	}
}

func TestClassifyTextDelta(t *testing.T) {
	line := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello "}}`)
	ev, ok := classify(line)
	if !ok || ev.Kind != EvAssistantDelta || ev.Text != "hello " {
		t.Fatalf("classify text_delta: got kind=%s text=%q ok=%v", ev.Kind, ev.Text, ok)
	}
}

func TestClassifyTextStartSwallowed(t *testing.T) {
	line := []byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_start"}}`)
	if _, ok := classify(line); ok {
		t.Fatal("text_start (no delta) must be swallowed")
	}
}

func TestClassifyAssistantFinal(t *testing.T) {
	line := []byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`)
	ev, ok := classify(line)
	if !ok || ev.Kind != EvAssistantFinal || ev.Text != "ab" {
		t.Fatalf("classify final: kind=%s text=%q ok=%v", ev.Kind, ev.Text, ok)
	}
}

func TestClassifyUserMessageEndSwallowed(t *testing.T) {
	line := []byte(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)
	if _, ok := classify(line); ok {
		t.Fatal("user message_end must be swallowed")
	}
}

func TestClassifyAssistantError(t *testing.T) {
	line := []byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}}`)
	ev, ok := classify(line)
	if !ok || ev.Kind != EvError {
		t.Fatalf("kind=%s ok=%v", ev.Kind, ok)
	}
	if !strings.Contains(ev.Text, "Overloaded") {
		t.Errorf("missing Overloaded: %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "overloaded_error") {
		t.Errorf("missing type prefix: %q", ev.Text)
	}
}

func TestClassifyBadJSON(t *testing.T) {
	ev, ok := classify([]byte(`{not json`))
	if !ok || ev.Kind != EvError {
		t.Fatalf("bad json: kind=%s ok=%v", ev.Kind, ok)
	}
}

func TestClassifyAgentEndSwallowed(t *testing.T) {
	if _, ok := classify([]byte(`{"type":"agent_end","messages":[]}`)); ok {
		t.Fatal("agent_end must be swallowed (EvDone is synthesised on exit)")
	}
}

func TestClassifyEmptyContentSwallowed(t *testing.T) {
	// Empty content list with stop=stop means "no text" — must NOT emit
	// an EvAssistantFinal with empty Text.
	line := []byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[]}}`)
	if _, ok := classify(line); ok {
		t.Fatal("empty content must be swallowed, not an empty final")
	}
}

// --- Start() integration tests against fake omp ---

func TestStartHappyPath(t *testing.T) {
	t.Setenv(helperEnv, "happy")

	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd:    dir,
		Prompt: "say hi",
		Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	var (
		gotSession string
		deltas     []string
		finalText  string
		gotDone    bool
		errs       []string
	)
	for ev := range r.Events() {
		switch ev.Kind {
		case EvSessionID:
			gotSession = ev.SessionID
		case EvAssistantDelta:
			deltas = append(deltas, ev.Text)
		case EvAssistantFinal:
			finalText = ev.Text
		case EvDone:
			gotDone = true
		case EvError:
			errs = append(errs, ev.Text)
		}
	}
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if gotSession != "test-session-uuid" {
		t.Errorf("session id: got %q", gotSession)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Errorf("deltas concat: got %q", strings.Join(deltas, ""))
	}
	if finalText != "Hello world" {
		t.Errorf("final text: got %q", finalText)
	}
	if !gotDone {
		t.Error("EvDone missing")
	}
}

func TestStartAPIErrorEvent(t *testing.T) {
	t.Setenv(helperEnv, "error")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var sawError bool
	for ev := range r.Events() {
		if ev.Kind == EvError && strings.Contains(ev.Text, "Overloaded") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected EvError mentioning Overloaded")
	}
}

func TestStartBadJSONLineEmitsError(t *testing.T) {
	t.Setenv(helperEnv, "bad-json")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var sawError, sawSession bool
	for ev := range r.Events() {
		switch ev.Kind {
		case EvError:
			sawError = true
		case EvSessionID:
			sawSession = true
		}
	}
	if !sawError {
		t.Error("malformed line should produce EvError")
	}
	if !sawSession {
		t.Error("parser must continue after a bad line and emit the valid session")
	}
}

func TestStartCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("cancellation test pauses up to 3s")
	}
	t.Setenv(helperEnv, "slow")
	t.Setenv(helperSleepEnv, "5000")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	drained := make(chan struct{})
	go func() {
		for range r.Events() {
		}
		close(drained)
	}()

	// Wait until the helper is up (session emitted).
	time.Sleep(200 * time.Millisecond)

	cancelStart := time.Now()
	r.Cancel(2 * time.Second)
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation didn't terminate the child within 3s")
	}
	elapsed := time.Since(cancelStart)
	if elapsed > 2500*time.Millisecond {
		t.Errorf("cancel took too long: %v", elapsed)
	}
}

func TestStartArgvIncludesAllFlags(t *testing.T) {
	t.Setenv(helperEnv, "happy")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd:       dir,
		Prompt:    "hi",
		SessionID: "sess-abc",
		Model:     "opus",
		Thinking:  "high",
		Binary:    helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for range r.Events() {
	}

	args := r.Cmd().Args
	// argv[0] is the binary itself. We assert presence of each flag.
	want := []string{"-p", "--mode", "json", "--resume", "sess-abc", "--model", "opus", "--thinking", "high", "hi"}
	for _, w := range want {
		if !contains(args, w) {
			t.Errorf("argv missing %q; got %v", w, args)
		}
	}
	if args[len(args)-1] != "hi" {
		t.Errorf("prompt must be last arg; got %v", args)
	}
}

func TestStartArgvOmitsBlanks(t *testing.T) {
	t.Setenv(helperEnv, "happy")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "hi", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for range r.Events() {
	}
	args := r.Cmd().Args
	for _, bad := range []string{"--resume", "--model", "--thinking"} {
		if contains(args, bad) {
			t.Errorf("blank options should be omitted; %q present in %v", bad, args)
		}
	}
}

func TestStartMissingCwd(t *testing.T) {
	_, err := Start(context.Background(), RunOptions{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error for missing Cwd")
	}
}

func TestStartMissingPrompt(t *testing.T) {
	_, err := Start(context.Background(), RunOptions{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for missing Prompt")
	}
}

func TestStartBinaryNotFound(t *testing.T) {
	_, err := Start(context.Background(), RunOptions{
		Cwd:    t.TempDir(),
		Prompt: "x",
		Binary: "/definitely/does/not/exist/omp-fake-xyzzy",
	})
	if err == nil {
		t.Fatal("expected spawn error for missing binary")
	}
}

func TestStartLogPathReceivesStderr(t *testing.T) {
	t.Setenv(helperEnv, "error")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t), LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for range r.Events() {
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestStartExitFailEmitsError(t *testing.T) {
	t.Setenv(helperEnv, "exit-fail")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var sawError, sawDone bool
	for ev := range r.Events() {
		switch ev.Kind {
		case EvError:
			sawError = true
		case EvDone:
			sawDone = true
		}
	}
	if !sawError {
		t.Error("non-zero exit should produce EvError")
	}
	if !sawDone {
		t.Error("EvDone must still close out the stream after EvError")
	}
}

func TestStartEmptyOutput(t *testing.T) {
	t.Setenv(helperEnv, "empty")
	dir := t.TempDir()
	r, err := Start(context.Background(), RunOptions{
		Cwd: dir, Prompt: "x", Binary: helperBinary(t),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var gotDone bool
	for ev := range r.Events() {
		if ev.Kind == EvDone {
			gotDone = true
		}
	}
	if !gotDone {
		t.Error("EvDone must fire even with no stdout")
	}
}

// --- helpers ---

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
