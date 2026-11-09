package refine

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Store holds previewed selections in memory (D6): at most MaxHeld, each for
// TTL, tied to the actor that previewed it. Only the paths and the content
// hash are kept, not the contents: a send re-reads the files and compares.
const (
	TTL     = 10 * time.Minute
	MaxHeld = 50
)

// Held is what the store keeps of one preview.
type Held struct {
	Actor    string
	Repo     string
	TextHash string // hash of the normalised requirement it was previewed with
	Paths    []string
	Hash     string // Selection.Hash at preview time
	Head     string
	Created  time.Time
}

// Store is safe for concurrent use. Now nil means time.Now.
type Store struct {
	mu  sync.Mutex
	m   map[string]Held
	Now func() time.Time
}

// NewStore returns an empty store.
func NewStore(now func() time.Time) *Store { return &Store{m: map[string]Held{}, Now: now} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// TextHash is the hash the store keeps of a requirement.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (s *Store) purge(now time.Time) {
	for id, h := range s.m {
		if now.Sub(h.Created) >= TTL {
			delete(s.m, id)
		}
	}
}

// Put stores the preview and returns its random selection id (128 random
// bits, independent of the content hash). Past MaxHeld the oldest is dropped.
func (s *Store) Put(actor, repo, text string, sel Selection) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("refine: no randomness")
	}
	id := hex.EncodeToString(b[:])
	paths := make([]string, len(sel.Files))
	for i, f := range sel.Files {
		paths[i] = f.Path
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge(now)
	for len(s.m) >= MaxHeld {
		oldest, first := "", true
		var at time.Time
		for k, h := range s.m {
			if first || h.Created.Before(at) {
				oldest, at, first = k, h.Created, false
			}
		}
		delete(s.m, oldest)
	}
	s.m[id] = Held{Actor: actor, Repo: repo, TextHash: TextHash(text), Paths: paths, Hash: sel.Hash, Head: sel.Head, Created: now}
	return id, nil
}

// Get returns the held preview for id when it is live and belongs to actor.
// Unknown, expired and someone else's ids are indistinguishable.
func (s *Store) Get(id, actor string) (Held, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge(now)
	h, ok := s.m[id]
	if !ok || h.Actor != actor {
		return Held{}, false
	}
	return h, true
}
