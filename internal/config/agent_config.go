package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentConfig is the per-repo agent settings file written by /model and
// /effort and read on every `omp -p` spawn. Lives at <repoPath>/.trd/agent.json.
//
// Empty fields cause the corresponding `omp` flag to be omitted, letting omp
// fall back to its own defaults (user-level config / env vars).
type AgentConfig struct {
	// Model is forwarded as `omp --model <Model>`. Fuzzy match — "opus",
	// "sonnet", provider-prefixed names all work.
	Model string `json:"model,omitempty"`
	// Thinking is forwarded as `omp --thinking <Thinking>`. Valid values:
	// "minimal", "low", "medium", "high", "xhigh". TRD historically called
	// this "effort"; the JSON field stays "thinking" to match the omp flag.
	Thinking string `json:"thinking,omitempty"`
	// UpdatedAt is RFC3339 of the last write. Informational only.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// agentConfigPath returns <repoPath>/.trd/agent.json (no I/O).
func agentConfigPath(repoPath string) string {
	return filepath.Join(repoPath, ".trd", "agent.json")
}

// ReadAgentConfig loads the per-repo agent config. A missing file returns
// the zero value with no error — callers treat zero as "no overrides".
func ReadAgentConfig(repoPath string) (AgentConfig, error) {
	var cfg AgentConfig
	data, err := os.ReadFile(agentConfigPath(repoPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse agent.json: %w", err)
	}
	return cfg, nil
}

// writeAgentConfig persists cfg to <repoPath>/.trd/agent.json, creating the
// directory if needed. Stamps UpdatedAt with the current time in UTC.
func writeAgentConfig(repoPath string, cfg AgentConfig) error {
	dir := filepath.Join(repoPath, ".trd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(agentConfigPath(repoPath), data, 0o600)
}

// WriteAgentField updates one field on the agent config and persists the
// rest unchanged. field is the JSON-tag name ("model", "thinking"); value
// is the new contents. An empty value clears the field.
//
// Unknown fields return an error rather than silently no-oping — callers
// validate against a whitelist before calling.
func WriteAgentField(repoPath, field, value string) error {
	cfg, err := ReadAgentConfig(repoPath)
	if err != nil {
		return err
	}
	switch strings.ToLower(field) {
	case "model":
		cfg.Model = value
	case "thinking", "effort":
		cfg.Thinking = value
	default:
		return fmt.Errorf("unknown agent.json field %q", field)
	}
	return writeAgentConfig(repoPath, cfg)
}
