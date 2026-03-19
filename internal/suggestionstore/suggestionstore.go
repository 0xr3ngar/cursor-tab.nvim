package suggestionstore

import (
	"sync"
	"time"
)

type RangeInfo struct {
	StartLine   int32 `json:"start_line"`
	StartColumn int32 `json:"start_column"`
	EndLine     int32 `json:"end_line"`
	EndColumn   int32 `json:"end_column"`
}

type Suggestion struct {
	Text                   string     `json:"text"`
	Range                  *RangeInfo `json:"range,omitempty"`
	BindingID              string     `json:"binding_id,omitempty"`
	ShouldRemoveLeadingEol bool       `json:"should_remove_leading_eol,omitempty"`
	NextSuggestionID       string     `json:"next_suggestion_id,omitempty"`
	SuggestionConfidence   *int32     `json:"suggestion_confidence,omitempty"`
	CreatedAt              time.Time  `json:"-"` // for TTL expiration
}

type Store struct {
	mu          sync.RWMutex
	suggestions map[string]*Suggestion
	maxSize     int
	ttl         time.Duration
	stopCleanup chan struct{}
}

func NewStore() *Store {
	s := &Store{
		suggestions: make(map[string]*Suggestion),
		maxSize:     50,
		ttl:         60 * time.Second,
		stopCleanup: make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// cleanupLoop periodically evicts expired entries
func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.evictExpired()
		case <-s.stopCleanup:
			return
		}
	}
}

// evictExpired removes entries older than TTL
func (s *Store) evictExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, sugg := range s.suggestions {
		if now.Sub(sugg.CreatedAt) > s.ttl {
			delete(s.suggestions, id)
		}
	}
}

func (s *Store) Store(suggestionID string, suggestion *Suggestion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	suggestion.CreatedAt = time.Now()
	s.suggestions[suggestionID] = suggestion

	// Enforce max size with FIFO eviction (evict oldest)
	if len(s.suggestions) > s.maxSize {
		var oldestID string
		var oldestTime time.Time
		for id, sugg := range s.suggestions {
			if oldestID == "" || sugg.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = sugg.CreatedAt
			}
		}
		if oldestID != "" {
			delete(s.suggestions, oldestID)
		}
	}
}

func (s *Store) Get(suggestionID string) *Suggestion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.suggestions[suggestionID]
}

func (s *Store) Delete(suggestionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.suggestions, suggestionID)
}

// ClearAll removes all entries from the store
func (s *Store) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suggestions = make(map[string]*Suggestion)
}

// Keys returns all suggestion IDs currently in the store (for debugging)
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.suggestions))
	for k := range s.suggestions {
		keys = append(keys, k)
	}
	return keys
}

// GetAll returns all suggestions currently in the store (for debugging)
func (s *Store) GetAll() map[string]*Suggestion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Make a copy to avoid race conditions
	all := make(map[string]*Suggestion, len(s.suggestions))
	for k, v := range s.suggestions {
		all[k] = v
	}
	return all
}
