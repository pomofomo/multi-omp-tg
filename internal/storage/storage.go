// Package storage is a thin bbolt wrapper for TRD instance state.
package storage

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketInstances       = []byte("instances")
	bucketByTopic         = []byte("by_topic")
	bucketBySecret        = []byte("by_secret")
	bucketAllowedUsers    = []byte("allowed_users")
	bucketSettings        = []byte("settings")
	bucketDeferredPrompts = []byte("deferred_prompts")
)

// settingLastUpdateID is the persisted Telegram long-poll cursor. Stored
// as a decimal-encoded int64 in the settings bucket so the successor of
// an in-place re-exec can resume polling without redelivering or losing
// updates that landed during the exec gap. See DEBUG.md
// "Proposal A — graceful in-process self-restart".
const settingLastUpdateID = "last_update_id"

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
	Controller  bool      `json:"controller,omitempty"`
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
		for _, b := range [][]byte{bucketInstances, bucketByTopic, bucketBySecret, bucketAllowedUsers, bucketSettings, bucketDeferredPrompts} {
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
		// Secret is unused after the headless-omp port (no WS auth). Older
		// rows may still carry one; we keep the index for those, but a
		// blank Secret on a fresh row is permitted and bypasses the index.
		if inst.Secret != "" {
			if err := bySecret.Put([]byte(inst.Secret), []byte(inst.InstanceID)); err != nil {
				return err
			}
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

// --- Long-poll cursor (for in-place self-restart) ---

// GetLastUpdateID returns the last Telegram update_id we acknowledged.
// Returns 0 when no cursor has been persisted yet (fresh DB or never
// restarted). See DEBUG.md "State that must survive an in-place exec".
func (s *Store) GetLastUpdateID() int {
	v := s.GetSetting(settingLastUpdateID)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// SetLastUpdateID persists the last acked Telegram update_id so a
// successor process can resume polling at update_id+1 without
// redelivering or skipping updates.
func (s *Store) SetLastUpdateID(id int) error {
	return s.SetSetting(settingLastUpdateID, strconv.Itoa(id))
}

// --- Deferred prompts (survive an in-place self-restart) ---

// DeferredPrompt is a Telegram-driven prompt that arrived while the
// dispatcher was draining for restart. The successor process drains the
// bucket on startup and re-routes each one through enqueueOrRun.
type DeferredPrompt struct {
	InstanceID string    `json:"instance_id"`
	ChatID     int64     `json:"chat_id"`
	ThreadID   int       `json:"thread_id"`
	MsgID      int       `json:"msg_id"`
	User       string    `json:"user,omitempty"`
	Text       string    `json:"text"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// EnqueueDeferred appends a prompt to the deferred queue. Keys are
// timestamp-prefixed monotonic ids so DrainDeferred returns items in
// FIFO order across processes.
func (s *Store) EnqueueDeferred(p DeferredPrompt) error {
	if p.EnqueuedAt.IsZero() {
		p.EnqueuedAt = time.Now().UTC()
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDeferredPrompts)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		// Key: 8-byte big-endian nanos | 8-byte big-endian sequence.
		// Sortable by time first (so successor drains oldest first) with
		// the sequence as tiebreaker for sub-nanosecond inserts.
		key := make([]byte, 16)
		binary.BigEndian.PutUint64(key[:8], uint64(p.EnqueuedAt.UnixNano()))
		binary.BigEndian.PutUint64(key[8:], seq)
		return b.Put(key, data)
	})
}

// DrainDeferred atomically reads and clears every deferred prompt,
// returning them in FIFO order. Items are deleted within the same
// transaction so a crash mid-drain at most loses (never duplicates) one
// pass — the successor of the successor will not redeliver them.
func (s *Store) DrainDeferred() ([]DeferredPrompt, error) {
	var out []DeferredPrompt
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDeferredPrompts)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var p DeferredPrompt
			if err := json.Unmarshal(v, &p); err != nil {
				// Skip malformed entry but log via the key being dropped.
				continue
			}
			out = append(out, p)
		}
		// Delete every key we just read. Recreate the bucket to wipe in
		// one shot — cheaper than per-key Delete on a long backlog.
		if err := tx.DeleteBucket(bucketDeferredPrompts); err != nil {
			return err
		}
		_, err := tx.CreateBucket(bucketDeferredPrompts)
		return err
	})
	return out, err
}

// DeferredCount returns the current size of the deferred queue without
// draining it. Useful for ops endpoints.
func (s *Store) DeferredCount() (int, error) {
	var n int
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketDeferredPrompts).Stats().KeyN
		return nil
	})
	return n, err
}
