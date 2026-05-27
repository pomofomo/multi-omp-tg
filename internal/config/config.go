// Package config handles paths and the per-repo .trd/config.json identity file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RepoConfig is written to <repo>/.trd/config.json on first clone.
// The channel plugin reads it to learn its identity and dispatcher address.
type RepoConfig struct {
	InstanceID     string `json:"instance_id"`
	Secret         string `json:"secret"`
	DispatcherPort int    `json:"dispatcher_port"`
}

// Root returns ~/.trd/.
func Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".trd"), nil
}

// StateDBPath returns the path to the bbolt database file.
func StateDBPath() (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "state.db"), nil
}

// LogPath returns the dispatcher log file path.
func LogPath() (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "trd.log"), nil
}

// ReposDir returns ~/.trd/repos/.
func ReposDir() (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "repos"), nil
}

// EnsureRoot creates ~/.trd/ and ~/.trd/repos/ if missing.
func EnsureRoot() error {
	r, err := Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r, "repos"), 0o700); err != nil {
		return err
	}
	return nil
}

// WriteRepoConfig writes <repoPath>/.trd/config.json with 0600 mode.
func WriteRepoConfig(repoPath string, cfg RepoConfig) error {
	dir := filepath.Join(repoPath, ".trd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return nil
}

// ReadRepoConfig loads <repoPath>/.trd/config.json.
func ReadRepoConfig(repoPath string) (RepoConfig, error) {
	var cfg RepoConfig
	data, err := os.ReadFile(filepath.Join(repoPath, ".trd", "config.json"))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// EnsureGitignore appends TRD-specific entries to <repoPath>/.gitignore if not
// already present: .trd/, .omc/. These are per-instance files that
// should not be committed to the repo.
func EnsureGitignore(repoPath string) error {
	path := filepath.Join(repoPath, ".gitignore")
	var existing string
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	}

	entries := []struct{ pattern, alt string }{
		{".trd/", ".trd"},
		{".omc/", ".omc"},
	}

	var toAdd []string
	for _, e := range entries {
		if containsLine(existing, e.pattern) || (e.alt != "" && containsLine(existing, e.alt)) {
			continue
		}
		toAdd = append(toAdd, e.pattern)
	}
	if len(toAdd) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		prefix = "\n"
	}
	for _, entry := range toAdd {
		_, err = f.WriteString(prefix + entry + "\n")
		if err != nil {
			return err
		}
		prefix = ""
	}
	return err
}

func containsLine(haystack, needle string) bool {
	start := 0
	for i := 0; i <= len(haystack); i++ {
		if i == len(haystack) || haystack[i] == '\n' {
			line := haystack[start:i]
			// trim trailing CR
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line == needle {
				return true
			}
			start = i + 1
		}
	}
	return false
}
