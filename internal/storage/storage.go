// Package storage is a thin bbolt wrapper for TRD instance state.
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketInstances    = []byte("instances")
	bucketByTopic      = []byte("by_topic")
	bucketBySecret     = []byte("by_secret")
	bucketAllowedUsers = []byte("allowed_users")
	bucketSettings     = []byte("settings")
)

// State is the running state of an instance.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// Instance is the row stored in the `instances` bucket.
type Instance struct {
	InstanceID  string    `json:"instance_id"`
	ChatID      int64     `json:"chat_id"`
	TopicID     int       `json:"topic_id"` // message_thread_id; 0 means no topic (General)
	RepoURL     string    `json:"repo_url"`
	RepoPath    string    `json:"repo_path"`
	RepoName    string    `json:"repo_name"`
	Secret      string    `json:"secret"`
	SessionID   string    `json:"session_id,omitempty"` // omp session id (captured from first NDJSON line of last run)
	State       State     `json:"state"`
	FailCount   int       `json:"fail_count"`
	Manager     bool      `json:"manager"`
	Debug       bool      `json:"debug,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RepoNameFromURL extracts a short repo name from a git URL.
// "git@github.com:org/repo.git" → "repo", "https://github.com/org/repo" → "repo".
func RepoNameFromURL(u string) string {
	// Strip trailing .git
	u = strings.TrimSuffix(u, ".git")
	// Take everything after the last / or :
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	if u == "" {
		return "unknown"
	}
	return u
}

// Store wraps a bbolt DB.
type Store struct {
	db *bolt.DB
}

// Open opens (or creates) the DB at path and ensures buckets exist.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketInstances, bucketByTopic, bucketBySecret, bucketAllowedUsers, bucketSettings} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close flushes and closes the DB.
func (s *Store) Close() error { return s.db.Close() }

func topicKey(chatID int64, topicID int) []byte {
	return []byte(fmt.Sprintf("%d:%d", chatID, topicID))
}

// Put writes an instance and updates secondary indexes.
// If an existing row under the same instance_id had a different secret or topic,
// the old index entries are cleaned up.
func (s *Store) Put(inst Instance) error {
	if inst.InstanceID == "" {
		return errors.New("instance_id required")
	}
	now := time.Now().UTC()
	inst.UpdatedAt = now
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = now
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		inst := inst
		insts := tx.Bucket(bucketInstances)
		byTopic := tx.Bucket(bucketByTopic)
		bySecret := tx.Bucket(bucketBySecret)
		// Clean stale indexes if this instance previously existed.
		if old := insts.Get([]byte(inst.InstanceID)); old != nil {
			var prev Instance
			if err := json.Unmarshal(old, &prev); err == nil {
				if prev.Secret != "" && prev.Secret != inst.Secret {
					_ = bySecret.Delete([]byte(prev.Secret))
				}
				if prev.ChatID != inst.ChatID || prev.TopicID != inst.TopicID {
					_ = byTopic.Delete(topicKey(prev.ChatID, prev.TopicID))
				}
			}
		}
		data, err := json.Marshal(inst)
		if err != nil {
			return err
		}
		if err := insts.Put([]byte(inst.InstanceID), data); err != nil {
			return err
		}
		if err := byTopic.Put(topicKey(inst.ChatID, inst.TopicID), []byte(inst.InstanceID)); err != nil {
			return err
		}
		if err := bySecret.Put([]byte(inst.Secret), []byte(inst.InstanceID)); err != nil {
			return err
		}
		return nil
	})
}

// Delete removes an instance and its index entries.
func (s *Store) Delete(instanceID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		insts := tx.Bucket(bucketInstances)
		byTopic := tx.Bucket(bucketByTopic)
		bySecret := tx.Bucket(bucketBySecret)
		data := insts.Get([]byte(instanceID))
		if data == nil {
			return nil
		}
		var prev Instance
		if err := json.Unmarshal(data, &prev); err == nil {
			_ = bySecret.Delete([]byte(prev.Secret))
			_ = byTopic.Delete(topicKey(prev.ChatID, prev.TopicID))
		}
		return insts.Delete([]byte(instanceID))
	})
}

// Get looks up an instance by ID. Returns (nil, nil) if missing.
func (s *Store) Get(instanceID string) (*Instance, error) {
	var out *Instance
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketInstances).Get([]byte(instanceID))
		if data == nil {
			return nil
		}
		var inst Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			return err
		}
		out = &inst
		return nil
	})
	return out, err
}

// ByTopic looks up an instance by (chat_id, topic_id).
func (s *Store) ByTopic(chatID int64, topicID int) (*Instance, error) {
	var out *Instance
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket(bucketByTopic).Get(topicKey(chatID, topicID))
		if id == nil {
			return nil
		}
		data := tx.Bucket(bucketInstances).Get(id)
		if data == nil {
			return nil
		}
		var inst Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			return err
		}
		out = &inst
		return nil
	})
	return out, err
}

// BySecret looks up an instance by secret.
func (s *Store) BySecret(secret string) (*Instance, error) {
	var out *Instance
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket(bucketBySecret).Get([]byte(secret))
		if id == nil {
			return nil
		}
		data := tx.Bucket(bucketInstances).Get(id)
		if data == nil {
			return nil
		}
		var inst Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			return err
		}
		out = &inst
		return nil
	})
	return out, err
}

// All returns every instance.
func (s *Store) All() ([]Instance, error) {
	var out []Instance
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketInstances).ForEach(func(_, v []byte) error {
			var inst Instance
			if err := json.Unmarshal(v, &inst); err != nil {
				return err
			}
			out = append(out, inst)
			return nil
		})
	})
	return out, err
}

// --- Allowed users (allowlist) ---

// AddAllowedUser adds a username to the allowlist. Case-insensitive (stored lowercase).
func (s *Store) AddAllowedUser(username string) error {
	username = strings.ToLower(strings.TrimPrefix(username, "@"))
	if username == "" {
		return errors.New("username required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAllowedUsers).Put([]byte(username), []byte("1"))
	})
}

// RemoveAllowedUser removes a username from the allowlist.
func (s *Store) RemoveAllowedUser(username string) error {
	username = strings.ToLower(strings.TrimPrefix(username, "@"))
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAllowedUsers).Delete([]byte(username))
	})
}

// ListAllowedUsers returns all usernames in the allowlist.
func (s *Store) ListAllowedUsers() ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAllowedUsers).ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	return out, err
}

// IsAllowedUser checks if a username is in the stored allowlist.
func (s *Store) IsAllowedUser(username string) bool {
	username = strings.ToLower(strings.TrimPrefix(username, "@"))
	var found bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		found = tx.Bucket(bucketAllowedUsers).Get([]byte(username)) != nil
		return nil
	})
	return found
}

// --- Settings (persistent key-value config) ---

// SetSetting stores a key-value setting.
func (s *Store) SetSetting(key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSettings).Put([]byte(key), []byte(value))
	})
}

// GetSetting retrieves a setting by key. Returns "" if not found.
func (s *Store) GetSetting(key string) string {
	var val string
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketSettings).Get([]byte(key)); v != nil {
			val = string(v)
		}
		return nil
	})
	return val
}

// AllSettings returns all stored settings as a map.
func (s *Store) AllSettings() (map[string]string, error) {
	out := map[string]string{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSettings).ForEach(func(k, v []byte) error {
			out[string(k)] = string(v)
			return nil
		})
	})
	return out, err
}
