package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type countingSessionAffinityStore struct {
	mu      sync.Mutex
	records []SessionAffinityRecord
	saves   int
}

func (s *countingSessionAffinityStore) Load(context.Context) ([]SessionAffinityRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SessionAffinityRecord(nil), s.records...), nil
}

func (s *countingSessionAffinityStore) Save(_ context.Context, records []SessionAffinityRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append([]SessionAffinityRecord(nil), records...)
	s.saves++
	return nil
}

func newPersistenceTestManager(t *testing.T, authIDs ...string) (*Manager, *SessionAffinitySelector) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: 24 * time.Hour})
	manager.SetSelector(affinity)
	t.Cleanup(affinity.Stop)
	for _, id := range authIDs {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "claude", Status: StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
	}
	return manager, affinity
}

func TestSessionCacheSnapshotRestoreHonorsRemainingTTL(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	now := time.Now()

	restored := cache.Restore([]SessionAffinityRecord{
		{Keys: []string{"claude::claude:live::m"}, AuthID: "auth-live", ExpiresAt: now.Add(30 * time.Minute)},
		{Keys: []string{"claude::claude:expired::m"}, AuthID: "auth-expired", ExpiresAt: now.Add(-time.Minute)},
		{Keys: []string{"claude::claude:long::m", "claude::claude:long-alias::m"}, AuthID: "auth-long", ExpiresAt: now.Add(48 * time.Hour)},
		{Keys: []string{"claude::claude:noauth::m"}, AuthID: "", ExpiresAt: now.Add(time.Hour)},
	}, now)
	if restored != 2 {
		t.Fatalf("restored = %d, want 2", restored)
	}
	if got, ok := cache.Get("claude::claude:live::m"); !ok || got != "auth-live" {
		t.Fatalf("live binding = %q, %v", got, ok)
	}
	if _, ok := cache.Get("claude::claude:expired::m"); ok {
		t.Fatal("expired binding was restored")
	}
	if got, ok := cache.Get("claude::claude:long-alias::m"); !ok || got != "auth-long" {
		t.Fatalf("alias binding = %q, %v", got, ok)
	}

	snapshot := cache.Snapshot(now)
	expires := make(map[string]time.Time, len(snapshot))
	for _, record := range snapshot {
		expires[record.AuthID] = record.ExpiresAt
	}
	if got := expires["auth-live"]; !got.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("live expiry = %s, want remaining TTL preserved", got)
	}
	if got := expires["auth-long"]; !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("long expiry = %s, want capped at current TTL", got)
	}
	// Entries that expire relative to a later clock are not snapshotted.
	if later := cache.Snapshot(now.Add(45 * time.Minute)); len(later) != 1 || later[0].AuthID != "auth-long" {
		t.Fatalf("snapshot at later clock = %+v, want only auth-long", later)
	}
}

func TestSessionCacheRestoreKeepsLiveBindings(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	now := time.Now()
	cache.Set("claude::claude:s1::m", "auth-new")
	cache.Restore([]SessionAffinityRecord{{Keys: []string{"claude::claude:s1::m"}, AuthID: "auth-old", ExpiresAt: now.Add(time.Minute)}}, now)
	if got, _ := cache.Get("claude::claude:s1::m"); got != "auth-new" {
		t.Fatalf("binding = %q, want live binding auth-new to win", got)
	}
}

func TestFileSessionAffinityStoreRoundTripDropsRemovedCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SessionAffinityStateFileName)
	store := NewFileSessionAffinityStore(path)

	manager, affinity := newPersistenceTestManager(t, "auth-a", "auth-b")
	affinity.cache.Set("mixed::claude:s1::m", "auth-a")
	affinity.cache.Set("mixed::claude:s2::m", "auth-b")
	affinity.cache.Set("mixed::claude:s3::m", "auth-gone")

	persister := NewSessionAffinityPersister(manager, store, time.Minute)
	if errFlush := persister.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush: %v", errFlush)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("state file missing: %v", errStat)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	saved, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load: %v", errLoad)
	}
	if len(saved) != 2 {
		t.Fatalf("saved %d records, want 2 (binding to unknown credential dropped)", len(saved))
	}

	// Restart: auth-b was removed while the process was down.
	restarted, restartedAffinity := newPersistenceTestManager(t, "auth-a")
	restoredCount, errRestore := NewSessionAffinityPersister(restarted, store, time.Minute).Restore(context.Background())
	if errRestore != nil {
		t.Fatalf("Restore: %v", errRestore)
	}
	if restoredCount != 1 {
		t.Fatalf("restored = %d, want 1", restoredCount)
	}
	if got, ok := restartedAffinity.cache.Get("mixed::claude:s1::m"); !ok || got != "auth-a" {
		t.Fatalf("s1 binding after restart = %q, %v; want auth-a", got, ok)
	}
	if _, ok := restartedAffinity.cache.Get("mixed::claude:s2::m"); ok {
		t.Fatal("binding to removed credential auth-b was restored")
	}
}

func TestSessionAffinityPersisterRestoreUsesClockAndSkipsUnchangedSaves(t *testing.T) {
	base := time.Now()
	store := &countingSessionAffinityStore{records: []SessionAffinityRecord{
		{Keys: []string{"mixed::claude:fresh::m"}, AuthID: "auth-a", ExpiresAt: base.Add(3 * time.Hour)},
		{Keys: []string{"mixed::claude:stale::m"}, AuthID: "auth-a", ExpiresAt: base.Add(time.Hour)},
	}}
	manager, affinity := newPersistenceTestManager(t, "auth-a")
	persister := NewSessionAffinityPersister(manager, store, time.Minute)
	// The process restarts two hours later: the stale entry's TTL has elapsed.
	persister.now = func() time.Time { return base.Add(2 * time.Hour) }

	restored, errRestore := persister.Restore(context.Background())
	if errRestore != nil {
		t.Fatalf("Restore: %v", errRestore)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	if _, ok := affinity.cache.Get("mixed::claude:stale::m"); ok {
		t.Fatal("expired binding restored")
	}

	if errFlush := persister.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush: %v", errFlush)
	}
	if store.saves != 0 {
		t.Fatalf("saves = %d, want 0 for unchanged bindings", store.saves)
	}
	affinity.cache.Set("mixed::claude:new::m", "auth-a")
	if errStop := persister.Stop(context.Background()); errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}
	if store.saves != 1 || len(store.records) != 2 {
		t.Fatalf("saves = %d records = %d, want one final save with 2 records", store.saves, len(store.records))
	}
}

func TestManagerSetSelectorCarriesSessionBindingsAcrossRebuild(t *testing.T) {
	manager, oldAffinity := newPersistenceTestManager(t, "auth-a", "auth-b")
	auths := []*Auth{{ID: "auth-a", Status: StatusActive}, {ID: "auth-b", Status: StatusActive}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"sess-reload"}}}
	first, errPick := oldAffinity.Pick(context.Background(), "mixed", "m", opts, auths)
	if errPick != nil || first == nil {
		t.Fatalf("Pick: %v", errPick)
	}

	maxRetries := 3
	rebuilt := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: 2 * time.Hour, MaxRetries: &maxRetries})
	t.Cleanup(rebuilt.Stop)
	manager.SetSelector(rebuilt)

	for i := 0; i < 4; i++ {
		picked, errRebuiltPick := rebuilt.Pick(context.Background(), "mixed", "m", cliproxyexecutor.Options{Headers: opts.Headers}, auths)
		if errRebuiltPick != nil || picked == nil || picked.ID != first.ID {
			t.Fatalf("pick %d after rebuild = %v (%v), want %s", i, picked, errRebuiltPick, first.ID)
		}
	}
}
