package extension

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureWritesWhenMissing(t *testing.T) {
	root := t.TempDir()
	path, err := Ensure(root)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := filepath.Join(root, "ext", "tg.ts"); path != want {
		t.Errorf("path: got %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, Source()) {
		t.Errorf("on-disk content does not match embedded source")
	}
}

func TestEnsureRewritesWhenDrifted(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	dst := filepath.Join(root, "ext", "tg.ts")
	if err := os.WriteFile(dst, []byte("// tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(root); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, Source()) {
		t.Errorf("Ensure did not rewrite drifted content")
	}
}

func TestEnsureIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root); err != nil {
		t.Fatalf("first: %v", err)
	}
	dst := filepath.Join(root, "ext", "tg.ts")
	st1, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Force mtime back so a rewrite would be visible.
	old := st1.ModTime().Add(-1)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(root); err != nil {
		t.Fatalf("second: %v", err)
	}
	st2, _ := os.Stat(dst)
	if !st2.ModTime().Equal(old) {
		t.Errorf("Ensure rewrote file even though content matched: mtime moved from %v to %v", old, st2.ModTime())
	}
}

func TestSourceMentionsTgReact(t *testing.T) {
	// Smoke check that the embedded blob is the actual extension and
	// hasn't been accidentally replaced.
	if !bytes.Contains(Source(), []byte("tg_react")) {
		t.Errorf("embedded extension is missing the tg_react tool name")
	}
	if !bytes.Contains(Source(), []byte("agent_start")) {
		t.Errorf("embedded extension is missing the agent_start auto-react hook")
	}
	if !bytes.Contains(Source(), []byte("/api/tg/react")) {
		t.Errorf("embedded extension is missing the dispatcher endpoint path")
	}
}

func TestSystemPromptAppend(t *testing.T) {
	body := []byte(SystemPromptAppend)
	// Must contain a newline so omp's --append-system-prompt resolver
	// treats it as literal text instead of stat()-ing it as a file path.
	if !bytes.Contains(body, []byte("\n")) {
		t.Errorf("SystemPromptAppend must contain a newline to avoid file-path probing")
	}
	// Restored from the pre-port channel/index.ts ACKNOWLEDGE pattern —
	// keep the imperative phrasing so the LLM gets a clear instruction
	// independent of the deterministic agent_start backstop.
	for _, want := range []string{
		"ACKNOWLEDGE",
		"react with 👍",
		"BEFORE you start processing",
		"voice messages which arrive after transcription delay",
		"tg_react",
		"REPLY WHEN DONE",
		"ASK QUESTIONS",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("SystemPromptAppend missing required phrase %q", want)
		}
	}
}
