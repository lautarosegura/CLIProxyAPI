package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// SessionAffinityStateFileName is the file, inside the auth directory, that stores
// persisted session affinity bindings. It deliberately avoids the .json extension so auth
// watchers and token stores never treat it as a credential.
const SessionAffinityStateFileName = "session-affinity.sab"

// DefaultSessionAffinityPersistInterval is how often changed bindings are saved.
const DefaultSessionAffinityPersistInterval = time.Minute

// SessionAffinityStore persists session affinity bindings across restarts.
type SessionAffinityStore interface {
	Load(context.Context) ([]SessionAffinityRecord, error)
	Save(context.Context, []SessionAffinityRecord) error
}

type sessionAffinityStateFile struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updated_at"`
	Records   []SessionAffinityRecord `json:"records"`
}

// FileSessionAffinityStore stores session affinity bindings in a single JSON document.
type FileSessionAffinityStore struct {
	mu   sync.Mutex
	path string
}

// NewFileSessionAffinityStore creates a file-backed store writing to path.
func NewFileSessionAffinityStore(path string) *FileSessionAffinityStore {
	return &FileSessionAffinityStore{path: strings.TrimSpace(path)}
}

// Path returns the backing file path.
func (s *FileSessionAffinityStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load reads persisted bindings. A missing or empty file is treated as empty state.
func (s *FileSessionAffinityStore) Load(ctx context.Context) ([]SessionAffinityRecord, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session affinity state: %w", errRead)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var envelope sessionAffinityStateFile
	if errUnmarshal := json.Unmarshal(data, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("parse session affinity state: %w", errUnmarshal)
	}
	return envelope.Records, nil
}

// Save atomically replaces the persisted bindings.
func (s *FileSessionAffinityStore) Save(ctx context.Context, records []SessionAffinityRecord) error {
	if s == nil || s.path == "" {
		return nil
	}
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
	}
	if records == nil {
		records = []SessionAffinityRecord{}
	}
	data, errMarshal := json.Marshal(sessionAffinityStateFile{
		Version:   1,
		UpdatedAt: time.Now().UTC(),
		Records:   records,
	})
	if errMarshal != nil {
		return fmt.Errorf("marshal session affinity state: %w", errMarshal)
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeStateFileAtomic(s.path, data, "session affinity state")
}

// writeStateFileAtomic writes data to a private temp file and renames it into place.
func writeStateFileAtomic(path string, data []byte, label string) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create %s directory: %w", label, errMkdir)
	}
	tmpFile, errCreate := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create %s temp file: %w", label, errCreate)
	}
	tmp := tmpFile.Name()
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		if errClose := tmpFile.Close(); errClose != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("write %s temp file: %w; close temp file: %v", label, errWrite, errClose)
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s temp file: %w", label, errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s temp file: %w", label, errClose)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s file: %w", label, errRename)
	}
	return nil
}

// adoptBindings copies the live explicit bindings of a previous selector so a hot-reload
// rebuild does not move established sessions to other credentials.
func (s *SessionAffinitySelector) adoptBindings(previous *SessionAffinitySelector, now time.Time) int {
	if s == nil || previous == nil || s == previous || s.cache == nil || previous.cache == nil {
		return 0
	}
	return s.cache.Restore(previous.cache.Snapshot(now), now)
}

func (m *Manager) authExists(authID string) bool {
	if m == nil || authID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[authID]
	return ok && auth != nil
}

func (m *Manager) sessionAffinitySelector() *SessionAffinitySelector {
	if m == nil {
		return nil
	}
	affinity, _ := m.Selector().(*SessionAffinitySelector)
	return affinity
}

func (m *Manager) filterSessionAffinityRecords(records []SessionAffinityRecord) []SessionAffinityRecord {
	filtered := make([]SessionAffinityRecord, 0, len(records))
	for _, record := range records {
		if m.authExists(strings.TrimSpace(record.AuthID)) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

// SessionAffinityPersister restores session affinity bindings on startup and saves them
// periodically and on shutdown. Saves are skipped while bindings are unchanged.
type SessionAffinityPersister struct {
	manager  *Manager
	store    SessionAffinityStore
	interval time.Duration
	now      func() time.Time

	mu           sync.Mutex
	lastSelector *SessionAffinitySelector
	lastVersion  uint64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewSessionAffinityPersister creates a persister. A non-positive interval selects
// DefaultSessionAffinityPersistInterval.
func NewSessionAffinityPersister(manager *Manager, store SessionAffinityStore, interval time.Duration) *SessionAffinityPersister {
	if interval <= 0 {
		interval = DefaultSessionAffinityPersistInterval
	}
	return &SessionAffinityPersister{
		manager:  manager,
		store:    store,
		interval: interval,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (p *SessionAffinityPersister) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Restore loads persisted bindings into the active session affinity selector, dropping
// expired entries and entries whose credential no longer exists. It returns the number of
// restored bindings.
func (p *SessionAffinityPersister) Restore(ctx context.Context) (int, error) {
	if p == nil || p.manager == nil || p.store == nil {
		return 0, nil
	}
	affinity := p.manager.sessionAffinitySelector()
	if affinity == nil || affinity.cache == nil {
		return 0, nil
	}
	records, errLoad := p.store.Load(ctx)
	if errLoad != nil {
		return 0, errLoad
	}
	restored := affinity.cache.Restore(p.manager.filterSessionAffinityRecords(records), p.currentTime())
	p.mu.Lock()
	p.lastSelector = affinity
	p.lastVersion = affinity.cache.Version()
	p.mu.Unlock()
	return restored, nil
}

// Flush saves the bindings of the active session affinity selector when they changed since
// the last save. It does nothing while session affinity is disabled.
func (p *SessionAffinityPersister) Flush(ctx context.Context) error {
	if p == nil || p.manager == nil || p.store == nil {
		return nil
	}
	affinity := p.manager.sessionAffinitySelector()
	if affinity == nil || affinity.cache == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	version := affinity.cache.Version()
	if affinity == p.lastSelector && version == p.lastVersion {
		return nil
	}
	records := p.manager.filterSessionAffinityRecords(affinity.cache.Snapshot(p.currentTime()))
	if errSave := p.store.Save(ctx, records); errSave != nil {
		return errSave
	}
	p.lastSelector = affinity
	p.lastVersion = version
	return nil
}

// Start launches the periodic save loop until ctx ends or Stop is called.
func (p *SessionAffinityPersister) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.startOnce.Do(func() {
		go func() {
			defer close(p.doneCh)
			ticker := time.NewTicker(p.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-p.stopCh:
					return
				case <-ticker.C:
					if errFlush := p.Flush(context.Background()); errFlush != nil {
						log.Warnf("failed to persist session affinity bindings: %v", errFlush)
					}
				}
			}
		}()
	})
}

// Stop ends the periodic save loop and performs a final save.
func (p *SessionAffinityPersister) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	// A persister that was never started has no loop to wait for.
	p.startOnce.Do(func() {
		close(p.doneCh)
	})
	select {
	case <-p.doneCh:
	case <-ctxDone(ctx):
	}
	return p.Flush(ctx)
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}
