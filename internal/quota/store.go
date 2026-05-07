package quota

import (
	"sort"
	"sync"
	"time"
)

// Store is an in-memory map of auth-id → most-recent quota Snapshot.
// Safe for concurrent use. The zero value is ready to use.
type Store struct {
	mu        sync.RWMutex
	snapshots map[string]Snapshot
}

// Default is the package-level singleton used by RecordResponse and the
// management handler. Tests can construct dedicated *Store values to avoid
// cross-test pollution.
var Default = &Store{}

// Put replaces the snapshot for authID with the provided samples. Calling
// with an empty samples slice clears any existing snapshot to avoid stale
// data lingering after a recovery.
func (s *Store) Put(authID string, samples []Sample) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots == nil {
		s.snapshots = make(map[string]Snapshot)
	}
	if len(samples) == 0 {
		delete(s.snapshots, authID)
		return
	}
	// Defensive copy so callers can't mutate the stored slice afterwards.
	cloned := make([]Sample, len(samples))
	copy(cloned, samples)
	s.snapshots[authID] = Snapshot{
		AuthID:     authID,
		Samples:    cloned,
		ObservedAt: time.Now().UTC(),
	}
}

// Get returns the snapshot for authID and whether one was found.
func (s *Store) Get(authID string) (Snapshot, bool) {
	if s == nil || authID == "" {
		return Snapshot{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[authID]
	if !ok {
		return Snapshot{}, false
	}
	cloned := make([]Sample, len(snap.Samples))
	copy(cloned, snap.Samples)
	snap.Samples = cloned
	return snap, true
}

// All returns all snapshots, sorted by AuthID for stable output.
func (s *Store) All() []Snapshot {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Snapshot, 0, len(s.snapshots))
	for _, snap := range s.snapshots {
		cloned := make([]Sample, len(snap.Samples))
		copy(cloned, snap.Samples)
		snap.Samples = cloned
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out
}
