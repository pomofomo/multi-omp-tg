package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		// SSH passthrough
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", false},
		{"git@github.com:org/repo", "git@github.com:org/repo.git", false},
		{"git@gitlab.com:deep/path.git", "git@gitlab.com:deep/path.git", false},

		// HTTPS → SSH
		{"https://github.com/org/repo", "git@github.com:org/repo.git", false},
		{"https://github.com/org/repo.git", "git@github.com:org/repo.git", false},
		{"http://github.com/org/repo", "git@github.com:org/repo.git", false},

		// Bare URL → SSH
		{"github.com/org/repo", "git@github.com:org/repo.git", false},
		{"github.com/org/repo.git", "git@github.com:org/repo.git", false},
		{"gitlab.com/org/repo", "git@gitlab.com:org/repo.git", false},

		// Errors
		{"-flag", "", true},
		{"--option=value", "", true},
		{"noslash", "", true},
		{"host/onlyone", "", true},
		{"", "", true},
		{"git@nocodon", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeRepoURL(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("normalizeRepoURL(%q) = %q, want error", tt.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeRepoURL(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	t.Run("short text returns one chunk", func(t *testing.T) {
		got := splitMessage("hi", 4000)
		if len(got) != 1 || got[0] != "hi" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("long text splits at newline boundary", func(t *testing.T) {
		// 100-char limit; pieces of 60 and 60.
		text := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60)
		chunks := splitMessage(text, 100)
		if len(chunks) != 2 {
			t.Fatalf("want 2 chunks, got %d", len(chunks))
		}
		if !strings.HasSuffix(chunks[0], "\n") {
			t.Errorf("first chunk should end on newline boundary: %q", chunks[0])
		}
	})
	t.Run("long text without good newline cuts at limit", func(t *testing.T) {
		text := strings.Repeat("a", 200)
		chunks := splitMessage(text, 100)
		if len(chunks) != 2 {
			t.Fatalf("want 2 chunks, got %d", len(chunks))
		}
		if len(chunks[0]) != 100 || len(chunks[1]) != 100 {
			t.Errorf("chunk lengths: %d, %d", len(chunks[0]), len(chunks[1]))
		}
	})
}

func TestLooksLikeModelName(t *testing.T) {
	for _, ok := range []string{"opus", "claude-opus-4-7", "anthropic/claude", "gpt-5.2"} {
		if !looksLikeModelName(ok) {
			t.Errorf("should accept %q", ok)
		}
	}
	for _, bad := range []string{"", "123", "-", "//"} {
		if looksLikeModelName(bad) {
			t.Errorf("should reject %q", bad)
		}
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	// Missing file → os.ErrNotExist
	if _, err := tailFile(path, 10, 1000); !os.IsNotExist(err) {
		t.Fatalf("missing file: got err %v", err)
	}

	// Short file → returned as-is
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := tailFile(path, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "c") {
		t.Errorf("expected full content, got %q", out)
	}

	// Long file → tailed by line count
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "line"+string(rune('A'+i%26)))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ = tailFile(path, 5, 1000)
	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) < 4 || len(got) > 6 {
		t.Errorf("want ~5 lines, got %d: %v", len(got), got)
	}

	// Byte cap also enforced
	out, _ = tailFile(path, 1000, 10)
	if len(out) > 10 {
		t.Errorf("byte cap exceeded: len=%d, content=%q", len(out), out)
	}
}

func TestShortIDAndPreview(t *testing.T) {
	if shortID("abc") != "abc" {
		t.Error("short id passthrough failed")
	}
	if shortID("0123456789abcdef") != "01234567" {
		t.Error("short id truncation failed")
	}
	long := strings.Repeat("x", 500)
	p := preview(long)
	if len(p) > 203 {
		t.Errorf("preview too long: %d", len(p))
	}
	if !strings.HasSuffix(p, "…") {
		t.Errorf("preview missing ellipsis: %q", p)
	}
	// Newlines collapse.
	if preview("a\nb") != `a\nb` {
		t.Errorf("preview should escape newlines, got %q", preview("a\nb"))
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "", "x", "y") != "x" {
		t.Error("should return first non-empty")
	}
	if firstNonEmpty("", "") != "" {
		t.Error("all empty should return empty")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Error("under limit passthrough")
	}
	if got := truncate("hello world", 5); !strings.HasPrefix(got, "hello") || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("got %q", got)
	}
}
