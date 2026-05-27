// Package config handles paths and per-repo state under ~/.trd/ and
// <repo>/.trd/. The per-repo files split into:
//
//   <repo>/.trd/agent.json — model/thinking overrides (agent_config.go)
//
// There used to be a <repo>/.trd/config.json carrying the WS secret and
// dispatcher port for the channel plugin; that file is obsolete now that
// `omp -p` is invoked directly per Telegram message.
package config

import (
	"os"
	"path/filepath"
)

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

// LogsDir returns ~/.trd/logs/ for per-instance agent stderr logs.
func LogsDir() (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "logs"), nil
}

// InstanceLogPath returns ~/.trd/logs/<instance-id>.log.
func InstanceLogPath(instanceID string) (string, error) {
	dir, err := LogsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, instanceID+".log"), nil
}

// ReposDir returns ~/.trd/repos/.
func ReposDir() (string, error) {
	r, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(r, "repos"), nil
}

// EnsureRoot creates ~/.trd/, ~/.trd/repos/, and ~/.trd/logs/ if missing.
func EnsureRoot() error {
	r, err := Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r, "repos"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r, "logs"), 0o700); err != nil {
		return err
	}
	return nil
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
