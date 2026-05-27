package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAgentConfigMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ReadAgentConfig(dir)
	if err != nil {
		t.Fatalf("missing file should be nil error, got %v", err)
	}
	if cfg != (AgentConfig{}) {
		t.Fatalf("want zero AgentConfig, got %+v", cfg)
	}
}

func TestWriteAgentFieldRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentField(dir, "model", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentField(dir, "thinking", "high"); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "opus" {
		t.Errorf("model: want opus, got %q", cfg.Model)
	}
	if cfg.Thinking != "high" {
		t.Errorf("thinking: want high, got %q", cfg.Thinking)
	}
	if cfg.UpdatedAt == "" {
		t.Errorf("UpdatedAt should be stamped, got empty")
	}

	// File must live under .trd/ and be 0600.
	info, err := os.Stat(filepath.Join(dir, ".trd", "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("want perm 0600, got %o", info.Mode().Perm())
	}
}

func TestWriteAgentFieldOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentField(dir, "model", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentField(dir, "model", "sonnet"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ReadAgentConfig(dir)
	if cfg.Model != "sonnet" {
		t.Errorf("model: want sonnet (overwritten), got %q", cfg.Model)
	}
}

func TestWriteAgentFieldEffortAlias(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAgentField(dir, "effort", "low"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ReadAgentConfig(dir)
	if cfg.Thinking != "low" {
		t.Errorf("effort alias should set thinking, got %q", cfg.Thinking)
	}
}

func TestWriteAgentFieldUnknown(t *testing.T) {
	dir := t.TempDir()
	err := WriteAgentField(dir, "bogus", "x")
	if err == nil {
		t.Fatal("want error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the bad field: %v", err)
	}
}

func TestAgentConfigJSONOmitempty(t *testing.T) {
	// An empty AgentConfig must serialise without `null` fields so it stays
	// human-edit friendly.
	data, err := json.Marshal(AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("empty AgentConfig should marshal to {}; got %s", data)
	}
}
