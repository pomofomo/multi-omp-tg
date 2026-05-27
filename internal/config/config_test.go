package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)



func TestEnsureGitignoreAppendsOnce(t *testing.T) {
	dir := t.TempDir()
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte("node_modules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(gi)
	content := string(data)
	if strings.Count(content, ".trd/") != 1 {
		t.Errorf("want exactly one .trd/ entry, got:\n%s", content)
	}
	if strings.Count(content, ".omc/") != 1 {
		t.Errorf("want exactly one .omc/ entry, got:\n%s", content)
	}
	if strings.Contains(content, ".mcp.json") {
		t.Errorf("post-port gitignore must not mention .mcp.json:\n%s", content)
	}
}

func TestEnsureGitignoreCreatesIfMissing(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, entry := range []string{".trd/", ".omc/"} {
		if !strings.Contains(content, entry) {
			t.Errorf("gitignore missing %s: %s", entry, content)
		}
	}
}

func TestEnsureGitignoreRecognizesBareTrd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".trd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureGitignore(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if strings.Count(string(data), ".trd") != 1 {
		t.Errorf("should not re-append .trd: %s", data)
	}
}
