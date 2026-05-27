package storage

import (
	"path/filepath"
	"testing"
)

func TestPutGetByAllThreeIndexes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	inst := Instance{
		InstanceID: "abc-1",
		ChatID:     -1001234,
		TopicID:    42,
		RepoURL:    "git@github.com:example/repo.git",
		RepoPath:   "/tmp/repo",
		Secret:     "s3cret",
		State:      StateRunning,
	}
	if err := s.Put(inst); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.Get("abc-1")
	if err != nil || got == nil || got.InstanceID != "abc-1" {
		t.Fatalf("Get returned %+v err=%v", got, err)
	}
	got, err = s.ByTopic(-1001234, 42)
	if err != nil || got == nil || got.InstanceID != "abc-1" {
		t.Fatalf("ByTopic returned %+v err=%v", got, err)
	}
	got, err = s.BySecret("s3cret")
	if err != nil || got == nil || got.InstanceID != "abc-1" {
		t.Fatalf("BySecret returned %+v err=%v", got, err)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be populated")
	}
}

func TestSessionIDAndDebugRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Older rows (no SessionID, no Debug) must decode with zero values.
	inst := Instance{
		InstanceID: "legacy",
		ChatID:     1,
		TopicID:    1,
		Secret:     "s",
		State:      StateRunning,
	}
	if err := s.Put(inst); err != nil {
		t.Fatalf("put legacy: %v", err)
	}
	got, _ := s.Get("legacy")
	if got == nil || got.SessionID != "" || got.Debug {
		t.Fatalf("legacy row decoded with non-zero new fields: %+v", got)
	}

	// New rows must round-trip both fields.
	inst2 := Instance{
		InstanceID: "newer",
		ChatID:     1,
		TopicID:    2,
		Secret:     "s2",
		State:      StateRunning,
		SessionID:  "019e6884-eec4-7000-880a-f1856985a2fd",
		Debug:      true,
	}
	if err := s.Put(inst2); err != nil {
		t.Fatalf("put newer: %v", err)
	}
	got, _ = s.Get("newer")
	if got == nil {
		t.Fatal("newer row missing")
	}
	if got.SessionID != inst2.SessionID {
		t.Errorf("SessionID round-trip: got %q want %q", got.SessionID, inst2.SessionID)
	}
	if !got.Debug {
		t.Errorf("Debug round-trip: want true, got false")
	}
}

func TestPutUpdatesStaleIndexes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	inst := Instance{InstanceID: "id", ChatID: 1, TopicID: 1, Secret: "old", State: StateRunning}
	if err := s.Put(inst); err != nil {
		t.Fatal(err)
	}
	inst.Secret = "new"
	inst.TopicID = 2
	if err := s.Put(inst); err != nil {
		t.Fatal(err)
	}

	if got, _ := s.BySecret("old"); got != nil {
		t.Error("old secret index should be gone")
	}
	if got, _ := s.BySecret("new"); got == nil {
		t.Error("new secret index should resolve")
	}
	if got, _ := s.ByTopic(1, 1); got != nil {
		t.Error("old topic index should be gone")
	}
	if got, _ := s.ByTopic(1, 2); got == nil {
		t.Error("new topic index should resolve")
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inst := Instance{InstanceID: "x", Secret: "sk", ChatID: 7, TopicID: 7}
	_ = s.Put(inst)
	if err := s.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("x"); got != nil {
		t.Error("Get after Delete should be nil")
	}
	if got, _ := s.BySecret("sk"); got != nil {
		t.Error("BySecret after Delete should be nil")
	}
	if got, _ := s.ByTopic(7, 7); got != nil {
		t.Error("ByTopic after Delete should be nil")
	}
}

func TestMissingReturnsNilNilError(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "test.db"))
	defer s.Close()
	got, err := s.Get("nope")
	if err != nil || got != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", got, err)
	}
}

func TestRepoNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@github.com:org/repo.git", "repo"},
		{"git@github.com:org/repo", "repo"},
		{"https://github.com/org/repo.git", "repo"},
		{"https://github.com/org/repo", "repo"},
		{"github.com/org/repo", "repo"},
		{"git@gitlab.com:deep/nested/repo.git", "repo"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		got := RepoNameFromURL(tt.url)
		if got != tt.want {
			t.Errorf("RepoNameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestRepoNameStoredAndRetrieved(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inst := Instance{
		InstanceID: "rn-1",
		ChatID:     1,
		Secret:     "s",
		RepoURL:    "git@github.com:org/myrepo.git",
		RepoName:   "myrepo",
	}
	if err := s.Put(inst); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("rn-1")
	if err != nil || got == nil {
		t.Fatalf("Get: %v, %v", got, err)
	}
	if got.RepoName != "myrepo" {
		t.Errorf("RepoName = %q, want %q", got.RepoName, "myrepo")
	}
}

func TestAll(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "test.db"))
	defer s.Close()
	_ = s.Put(Instance{InstanceID: "a", Secret: "1", ChatID: 1})
	_ = s.Put(Instance{InstanceID: "b", Secret: "2", ChatID: 2})
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("want 2 instances, got %d", len(all))
	}
}
