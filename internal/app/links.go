package app

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	linkTTL          = 24 * time.Hour
	maxLinksPerVideo = 8
)

// linkEntry is one outstanding secret link. Only the SHA-256 hash of the
// token is stored or logged. JobCreatedAt records which run of the video the
// link belongs to: deleting and re-downloading creates a new run, and links
// from the old run never validate again.
type linkEntry struct {
	TokenHash    string    `json:"token_hash"`
	VideoID      string    `json:"video_id"`
	JobCreatedAt time.Time `json:"job_created_at"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// LinkStore issues and verifies share links, persisted atomically.
type LinkStore struct {
	mu    sync.Mutex
	path  string
	links map[string]linkEntry
	now   func() time.Time
}

func NewLinkStore(path string, now func() time.Time) *LinkStore {
	if now == nil {
		now = time.Now
	}
	return &LinkStore{path: path, links: map[string]linkEntry{}, now: now}
}

func (s *LinkStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []linkEntry
	if err := jsonUnmarshalStrict(data, &entries); err != nil {
		return err
	}
	fresh := map[string]linkEntry{}
	for _, entry := range entries {
		if entry.ExpiresAt.After(s.now()) && entry.TokenHash != "" && entry.VideoID != "" {
			fresh[entry.TokenHash] = entry
		}
	}
	s.links = fresh
	return nil
}

func (s *LinkStore) persistLocked() error {
	entries := make([]linkEntry, 0, len(s.links))
	for _, entry := range s.links {
		entries = append(entries, entry)
	}
	data, err := jsonMarshalStrict(entries)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileSync(dir, filepath.Base(s.path), data)
}

// Issue creates a fresh secret link for one completed video.
func (s *LinkStore) Issue(videoID string, jobCreatedAt time.Time) (token string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	var mine []linkEntry
	for _, entry := range s.links {
		if entry.VideoID == videoID {
			mine = append(mine, entry)
		}
	}
	if len(mine) >= maxLinksPerVideo {
		var oldestKey string
		var oldest time.Time
		for _, entry := range mine {
			if oldestKey == "" || entry.CreatedAt.Before(oldest) {
				oldestKey, oldest = entry.TokenHash, entry.CreatedAt
			}
		}
		delete(s.links, oldestKey)
	}
	token = randomToken(32)
	entry := linkEntry{
		TokenHash:    hashToken(token),
		VideoID:      videoID,
		JobCreatedAt: jobCreatedAt,
		CreatedAt:    now,
		ExpiresAt:    now.Add(linkTTL),
	}
	s.links[entry.TokenHash] = entry
	if err := s.persistLocked(); err != nil {
		delete(s.links, entry.TokenHash)
		return "", time.Time{}, err
	}
	return token, entry.ExpiresAt, nil
}

// Lookup validates a share token. Invalid, expired, and revoked links are
// indistinguishable. The job creation time is returned for the incarnation
// check: a link only opens the exact run it was issued for.
func (s *LinkStore) Lookup(token string) (videoID string, jobCreatedAt time.Time, ok bool) {
	if token == "" {
		return "", time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.links[hashToken(token)]
	if !found || !entry.ExpiresAt.After(s.now()) {
		return "", time.Time{}, false
	}
	return entry.VideoID, entry.JobCreatedAt, true
}

// Revoke drops every link for a video (deletion).
func (s *LinkStore) Revoke(videoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for key, entry := range s.links {
		if entry.VideoID == videoID {
			delete(s.links, key)
			changed = true
		}
	}
	if changed {
		_ = s.persistLocked()
	}
}

// Prune drops expired links; called from a background loop.
func (s *LinkStore) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
}

func (s *LinkStore) pruneLocked(now time.Time) {
	expired := false
	for key, entry := range s.links {
		if !entry.ExpiresAt.After(now) {
			delete(s.links, key)
			expired = true
		}
	}
	if expired {
		_ = s.persistLocked()
	}
}
