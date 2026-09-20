package app

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

// downloadFunc adapts a function to the Downloader interface.
type downloadFunc func(ctx context.Context, j Job, report Reporter) (DownloadResult, error)

func (f downloadFunc) Download(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
	return f(ctx, j, report)
}

// fetchFunc adapts a function to the MetadataFetcher interface.
type fetchFunc func(ctx context.Context, id string) (Metadata, error)

func (f fetchFunc) Fetch(ctx context.Context, id string) (Metadata, error) { return f(ctx, id) }

// memoryStore is an in-memory Store with injectable failures.
type memoryStore struct {
	mu        sync.Mutex
	jobs      map[string]Job
	files     map[string][]byte
	order     []string // insertion order of jobs, for stable scans
	saveErr   error
	removeErr error
	scanErr   error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]Job{}, files: map[string][]byte{}}
}

func (s *memoryStore) Save(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	if _, exists := s.jobs[j.ID]; !exists {
		s.order = append(s.order, j.ID)
	}
	s.jobs[j.ID] = j
	return nil
}

func (s *memoryStore) Load(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

func (s *memoryStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removeErr != nil {
		return s.removeErr
	}
	if _, ok := s.jobs[id]; !ok {
		return ErrNotFound
	}
	delete(s.jobs, id)
	delete(s.files, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

func (s *memoryStore) Scan() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	jobs := make([]Job, 0, len(s.order))
	for _, id := range s.order {
		jobs = append(jobs, s.jobs[id])
	}
	return jobs, nil
}

func (s *memoryStore) OpenVideo(id string) (File, time.Time, error) {
	s.mu.Lock()
	data, ok := s.files[id]
	s.mu.Unlock()
	if !ok || len(data) == 0 {
		return nil, time.Time{}, ErrNotFound
	}
	return memFile{bytes.NewReader(data)}, time.Now(), nil
}

func (s *memoryStore) VideoSize(id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[id]
	if !ok {
		return 0, ErrNotFound
	}
	return int64(len(data)), nil
}

func (s *memoryStore) Total() (int64, error) {
	jobs, err := s.Scan()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, j := range jobs {
		if j.Status != StatusReady {
			continue
		}
		if size, err := s.VideoSize(j.ID); err == nil {
			total += size
		}
	}
	return total, nil
}

// setFile publishes the video bytes a fake downloader would have produced.
func (s *memoryStore) setFile(id string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[id] = data
}

// saveGate wraps a Store, holding matching Saves until released. A test arms
// it and then decides exactly when a metadata write lands relative to a
// deletion — the interleaving the resurrection race lives in.
type saveGate struct {
	inner   Store
	when    func(Job) bool // which Saves to hold once armed
	armed   atomic.Bool
	entered chan struct{} // signals that a gated Save is in flight
	release chan struct{} // closed to let gated Saves through
}

func newSaveGate(inner Store, when func(Job) bool) *saveGate {
	return &saveGate{
		inner: inner, when: when,
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
}

func (g *saveGate) arm() { g.armed.Store(true) }

func (g *saveGate) Save(j Job) error {
	if g.armed.Load() && g.when(j) {
		g.entered <- struct{}{} // buffered; never blocks the save itself
		<-g.release
	}
	return g.inner.Save(j)
}

func (g *saveGate) Load(id string) (Job, error) { return g.inner.Load(id) }
func (g *saveGate) Remove(id string) error      { return g.inner.Remove(id) }
func (g *saveGate) Scan() ([]Job, error)        { return g.inner.Scan() }
func (g *saveGate) OpenVideo(id string) (File, time.Time, error) {
	return g.inner.OpenVideo(id)
}
func (g *saveGate) VideoSize(id string) (int64, error) { return g.inner.VideoSize(id) }
func (g *saveGate) Total() (int64, error)              { return g.inner.Total() }

type memFile struct{ r *bytes.Reader }

func (m memFile) Read(p []byte) (int, error) { return m.r.Read(p) }
func (m memFile) Seek(o int64, whence int) (int64, error) {
	return m.r.Seek(o, whence)
}
func (m memFile) Close() error { return nil }

var _ File = memFile{}

// clock is a controllable time source for expiration tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(start time.Time) *clock { return &clock{t: start} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// waitFor polls until fn succeeds or the deadline passes.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// videoID returns a valid 11-character YouTube ID for tests.
func videoID(n byte) string {
	id := make([]byte, 11)
	for i := range id {
		id[i] = 'a' + (n+byte(i))%26
	}
	return string(id)
}

// silentDownloader reports nothing and succeeds instantly.
func silentDownloader() downloadFunc {
	return func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		return DownloadResult{Title: "Test Video", SizeBytes: 1000}, nil
	}
}
