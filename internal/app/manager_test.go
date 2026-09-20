package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestManager wires a manager with an injectable clock, starting it so
// tests can drive it directly.
func newTestManager(store Store, dl Downloader, fetch MetadataFetcher) *Manager {
	m := NewManager(store, dl, fetch)
	base := time.Unix(1700000000, 0)
	var n atomic.Int64
	m.now = func() time.Time { return base.Add(time.Duration(n.Add(1)) * time.Millisecond) }
	return m
}

// blockedDownload waits until released or the job is cancelled.
func blockedDownload(calls *atomic.Int64, released chan struct{}, onEnter func()) downloadFunc {
	return func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		if onEnter != nil {
			onEnter()
		}
		select {
		case <-released:
			return DownloadResult{Title: "Test Video " + j.ID, SizeBytes: int64(1000 + len(j.ID))}, nil
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		}
	}
}

func TestSubmitDeduplicates(t *testing.T) {
	store := newMemoryStore()
	var calls atomic.Int64
	release := make(chan struct{})
	m := newTestManager(store, blockedDownload(&calls, release, nil), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{}, errors.New("network down")
	}))
	defer m.Close()

	id := videoID(1)
	v1, dup, err := m.Submit(id)
	if err != nil || dup {
		t.Fatalf("first submit = %+v, %v, %v", v1, dup, err)
	}
	// While the first job is in flight, resubmitting returns the same card.
	for i := 0; i < 5; i++ {
		v, dup, err := m.Submit(id)
		if err != nil || !dup {
			t.Fatalf("resubmit = %+v, dup=%v, %v", v, dup, err)
		}
		if v.ID != id {
			t.Fatalf("resubmit returned different job %q", v.ID)
		}
	}
	waitFor(t, "download to start", func() bool { return calls.Load() == 1 })
	if calls.Load() != 1 {
		t.Fatalf("downloader ran %d times; want 1", calls.Load())
	}
	close(release)
	waitFor(t, "job ready", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	// Submitting a ready job also deduplicates.
	if _, dup, err := m.Submit(id); err != nil || !dup {
		t.Errorf("submit after ready: dup=%v err=%v", dup, err)
	}
	if calls.Load() != 1 {
		t.Errorf("downloader ran %d times after resubmits; want 1", calls.Load())
	}
}

func TestSubmitResubmitsFailedJob(t *testing.T) {
	store := newMemoryStore()
	var calls atomic.Int64
	failFirst := true
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		calls.Add(1)
		if failFirst {
			failFirst = false
			return DownloadResult{Title: "Known Title"}, errors.New("yt-dlp exploded with /tmp/secret-path")
		}
		return DownloadResult{Title: "Known Title", SizeBytes: 500}, nil
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		// Transient failures keep the title pool from failing the job, so
		// the downloader's own error path is what runs here.
		return Metadata{}, errors.New("network down")
	}))
	defer m.Close()

	id := videoID(2)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job to fail", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusFailed
	})
	v, _ := m.Get(id)
	if v.Error == "" || v.Error == "yt-dlp exploded with /tmp/secret-path" {
		t.Errorf("internal error leaked: %q", v.Error)
	}
	if v.Error != "Unable to download this video. It may be unavailable, private, or age-restricted." {
		t.Errorf("unexpected error text: %q", v.Error)
	}
	if v.Title != "Known Title" {
		t.Errorf("title lost on failure: %q", v.Title)
	}
	// A failed job keeps its card but is resubmittable.
	if _, dup, err := m.Submit(id); err != nil || !dup {
		t.Fatalf("resubmit of failed = %v, %v", dup, err)
	}
	waitFor(t, "retry to succeed", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	if calls.Load() != 2 {
		t.Errorf("downloader ran %d times; want 2", calls.Load())
	}
	persisted, err := store.Load(id)
	if err != nil || persisted.Status != StatusReady || persisted.Title != "Known Title" {
		t.Errorf("persisted state = %+v, %v", persisted, err)
	}
}

func TestQueueAdmissionLimit(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	var calls atomic.Int64
	m := newTestManager(store, blockedDownload(&calls, release, nil), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "T"}, nil
	}))
	defer m.Close()
	defer close(release)

	var admitted []string
	for i := 0; i < maxUnfinishedJobs; i++ {
		id := videoID(byte(3 + i))
		if _, _, err := m.Submit(id); err != nil {
			t.Fatalf("submit %d rejected: %v", i, err)
		}
		admitted = append(admitted, id)
	}
	if _, _, err := m.Submit(videoID(25)); err != ErrBusy {
		t.Errorf("17th unfinished submit = %v; want ErrBusy", err)
	}
}

func TestSingleDownloadWorker(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	var calls atomic.Int64
	entered := make(chan struct{}, 2)
	m := newTestManager(store, blockedDownload(&calls, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "T"}, nil
	}))
	defer m.Close()

	idA, idB := videoID(40), videoID(41)
	if _, _, err := m.Submit(idA); err != nil {
		t.Fatal(err)
	}
	<-entered // worker busy with A
	if _, _, err := m.Submit(idB); err != nil {
		t.Fatal(err)
	}
	// While A is blocked, B must not start.
	deadline := time.After(300 * time.Millisecond)
	select {
	case <-entered:
		t.Fatal("second job started while the first was still active")
	case <-deadline:
	}
	close(release)
	waitFor(t, "both jobs ready", func() bool {
		a, _ := m.Get(idA)
		b, _ := m.Get(idB)
		return a.Status == StatusReady && b.Status == StatusReady
	})
}

func TestEarlyTitleBeforeDownloadStarts(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(nil, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "Early Title " + id, DurationSec: 42}, nil
	}))
	defer m.Close()

	// Keep the single download worker busy so the new job stays queued and
	// the title pool owns it.
	if _, _, err := m.Submit(videoID(49)); err != nil {
		t.Fatal(err)
	}
	<-entered

	id := videoID(50)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	// The title must arrive while the job is still waiting to download.
	waitFor(t, "title to arrive", func() bool {
		v, _ := m.Get(id)
		return v.Title == "Early Title "+id
	})
	v, _ := m.Get(id)
	if v.Status != StatusQueued {
		t.Fatalf("title arrived but status is %q; want queued", v.Status)
	}
	// The title pool's duration is persisted alongside the title, so both
	// survive a restart even if the download never finishes.
	waitFor(t, "title persisted", func() bool {
		j, err := store.Load(id)
		return err == nil && j.Title == "Early Title "+id && j.DurationSec == 42
	})
}

func TestTitlePoolIndependentOfDownloadWorker(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(&calls, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "Title for " + id}, nil
	}))
	defer m.Close()
	defer close(release)

	idA, idB := videoID(60), videoID(61)
	if _, _, err := m.Submit(idA); err != nil {
		t.Fatal(err)
	}
	<-entered // the single download worker is busy with A
	if _, _, err := m.Submit(idB); err != nil {
		t.Fatal(err)
	}
	// B is queued behind A, but its title arrives immediately.
	waitFor(t, "queued job title", func() bool {
		v, _ := m.Get(idB)
		return v.Title == "Title for "+idB && v.Status == StatusQueued
	})
}

func TestLiveStreamFailsJobBeforeDownload(t *testing.T) {
	store := newMemoryStore()
	var calls atomic.Int64
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	liveID := videoID(70)
	// Only the live video is rejected. The job that occupies the download
	// worker must fetch ordinary metadata, or the title pool could fail it
	// before the worker ever picks it up and `entered` would never fire.
	m := newTestManager(store, blockedDownload(&calls, release, func() { entered <- struct{}{} }), fetchFunc(func(_ context.Context, id string) (Metadata, error) {
		if id == liveID {
			return Metadata{}, DownloadError("Live and upcoming streams are not supported.")
		}
		return Metadata{Title: "Occupier", LiveStatus: "not_live"}, nil
	}))
	defer m.Close()

	// Keep the download worker busy so the pool can reject the live video
	// before any download attempt is made.
	if _, _, err := m.Submit(videoID(69)); err != nil {
		t.Fatal(err)
	}
	<-entered

	if _, _, err := m.Submit(liveID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live job to fail", func() bool {
		v, _ := m.Get(liveID)
		return v.Status == StatusFailed
	})
	v, _ := m.Get(liveID)
	if v.Error != "Live and upcoming streams are not supported." {
		t.Errorf("error = %q", v.Error)
	}
	if calls.Load() != 1 { // only the worker-occupying job
		t.Errorf("downloader ran %d times; want 1", calls.Load())
	}
}

func silentDownloaderWith(calls *atomic.Int64) downloadFunc {
	return func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		return DownloadResult{Title: "Test Video", SizeBytes: 1000}, nil
	}
}

func TestTitleFetchFailureFallsBackToDownloader(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		if j.Title != "" {
			t.Errorf("downloader received title %q despite fetch failures", j.Title)
		}
		return DownloadResult{Title: "Late Title", SizeBytes: 10}, nil
	}), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{}, errors.New("network hiccup")
	}))
	defer m.Close()

	id := videoID(80)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job ready via downloader fallback", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady && v.Title == "Late Title"
	})
}

func TestProgressStagesReported(t *testing.T) {
	store := newMemoryStore()
	type step struct {
		stage    string
		progress float64
		known    bool
	}
	steps := []step{
		{StageDownloadVideo, 10, true},
		{StageDownloadVideo, 55, true},
		{StageDownloadAudio, 25, true},
		{StageProcessing, 40, true},
		{StageVerifying, 0, false},
	}
	var sent = make(chan step, len(steps))
	var ack = make(chan struct{})
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		for _, s := range steps {
			report(s.stage, s.progress, s.known)
			sent <- s
			<-ack
		}
		return DownloadResult{Title: "Staged", SizeBytes: 321}, nil
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "Staged"}, nil
	}))
	defer m.Close()

	id := videoID(90)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(steps); i++ {
		s := <-sent
		v, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Stage != s.stage {
			t.Errorf("step %d: stage = %q; want %q", i, v.Stage, s.stage)
		}
		if v.Status != statusForStage(s.stage) {
			t.Errorf("step %d: status = %q; want %q", i, v.Status, statusForStage(s.stage))
		}
		if s.known && (v.Progress == nil || *v.Progress != s.progress) {
			t.Errorf("step %d: progress = %v; want %v", i, v.Progress, s.progress)
		}
		if !s.known && v.Progress != nil {
			t.Errorf("step %d: progress = %v; want nil", i, *v.Progress)
		}
		ack <- struct{}{}
	}
	waitFor(t, "ready", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	v, _ := m.Get(id)
	if v.Progress == nil || *v.Progress != 100 || v.SizeBytes != 321 {
		t.Errorf("final view = %+v", v)
	}
	if v.Title != "Staged" {
		t.Errorf("title = %q", v.Title)
	}
}

func TestRetryLifecycle(t *testing.T) {
	store := newMemoryStore()
	fail := true
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		if fail {
			fail = false
			return DownloadResult{}, DownloadError("The video is private.")
		}
		return DownloadResult{Title: "Retry", SizeBytes: 7}, nil
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "Retry"}, nil
	}))
	defer m.Close()

	id := videoID(100)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failure", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusFailed
	})
	if _, err := m.Retry(videoID(101)); err != ErrNotFound {
		t.Errorf("retry of unknown job = %v", err)
	}
	// While queued after retry, a second retry conflicts.
	v, err := m.Retry(id)
	if err != nil || v.Status != StatusQueued {
		t.Fatalf("retry = %+v, %v", v, err)
	}
	if _, err := m.Retry(id); err != ErrConflict {
		t.Errorf("retry of non-failed job = %v; want ErrConflict", err)
	}
	waitFor(t, "ready after retry", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
}

func TestDeleteActiveJobCancelsDownload(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	returned := make(chan struct{}, 1)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		calls.Add(1)
		entered <- struct{}{}
		defer func() { returned <- struct{}{} }()
		select {
		case <-release:
			return DownloadResult{Title: "x", SizeBytes: 1}, nil
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		}
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(110)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	// Delete must not return until the downloader has actually stopped.
	select {
	case err := <-delDone:
		t.Fatalf("Delete returned before the downloader stopped: %v", err)
	case <-returned:
	}
	if err := <-delDone; err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v; want ErrNotFound", err)
	}
	if _, _, err := m.Submit(id); err != nil {
		t.Errorf("submit after delete: %v", err)
	}
}

func TestDeleteQueuedJobNeverStarts(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(&calls, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	idA, idB := videoID(120), videoID(121)
	if _, _, err := m.Submit(idA); err != nil {
		t.Fatal(err)
	}
	<-entered // worker occupied, so B stays queued
	if _, _, err := m.Submit(idB); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B queued", func() bool {
		v, _ := m.Get(idB)
		return v.Status == StatusQueued
	})
	if err := m.Delete(idB); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(idB); err != ErrNotFound {
		t.Errorf("queued job not removed: %v", err)
	}
	close(release)
	waitFor(t, "A ready", func() bool {
		v, _ := m.Get(idA)
		return v.Status == StatusReady
	})
	if calls.Load() != 1 {
		t.Errorf("downloader ran %d times; want 1", calls.Load())
	}
}

func TestDeleteReadyJobRemovesFileAndTotal(t *testing.T) {
	root := t.TempDir()
	store := DiskStore{Root: root}
	// A real download publishes a file; emulate that by writing it during the
	// fake download.
	m := newTestManager(store, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		data := []byte("0123456789")
		if err := os.MkdirAll(filepath.Join(root, "videos", j.ID), 0o700); err != nil {
			return DownloadResult{}, err
		}
		if err := os.WriteFile(filepath.Join(root, "videos", j.ID, "video.mp4"), data, 0o600); err != nil {
			return DownloadResult{}, err
		}
		return DownloadResult{Title: "Ready", SizeBytes: int64(len(data))}, nil
	}), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(130)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ready", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	_, total, _ := m.List()
	if total != 10 {
		t.Errorf("total = %d; want 10", total)
	}
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "videos", id, "video.mp4")); !os.IsNotExist(err) {
		t.Errorf("video file survived: %v", err)
	}
	_, total, _ = m.List()
	if total != 0 {
		t.Errorf("total after delete = %d; want 0", total)
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v", err)
	}
}

func TestConcurrentDeletes(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return DownloadResult{}, nil
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		}
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(140)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = m.Delete(id)
		}(i)
	}
	wg.Wait()
	// Every call must succeed or report the job already gone — never a
	// storage or timeout failure — and at least one must have succeeded.
	succeeded := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrNotFound):
			// lost the race to another delete; the job is gone either way
		default:
			t.Errorf("concurrent delete: %v", err)
		}
	}
	if succeeded == 0 {
		t.Error("no concurrent delete succeeded")
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after deletes = %v", err)
	}
}

func TestDeleteDuringTitleFetch(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(nil, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		// Only the queued job's fetch matters here; the wind-down delay makes
		// the deletion window observable.
		<-ctx.Done()
		time.Sleep(250 * time.Millisecond)
		return Metadata{}, ctx.Err()
	}))
	defer m.Close()

	// Keep the download worker busy; the queued job is owned solely by the
	// title pool, so its worker must complete the deletion.
	if _, _, err := m.Submit(videoID(149)); err != nil {
		t.Fatal(err)
	}
	<-entered

	id := videoID(150)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fetch to start", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.jobs[id] != nil && m.jobs[id].fetching
	})
	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	select {
	case err := <-delDone:
		t.Fatalf("Delete returned before the fetcher stopped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed after the fetcher stopped")
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v", err)
	}
}

func TestCloseInterruptsActiveJobPersistedAsRetryable(t *testing.T) {
	store := newMemoryStore()
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		<-ctx.Done()
		return DownloadResult{}, ctx.Err()
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))

	id := videoID(160)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "download running", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusDownloading
	})
	m.Close()
	j, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusFailed || j.Error == "" {
		t.Errorf("interrupted job persisted as %+v", j)
	}
	if v, err := m.Get(id); err != nil || v.Status != StatusFailed {
		t.Errorf("post-close view = %+v, %v", v, err)
	}
}

func TestSaveFailureMarksJobFailedButDeletable(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(nil, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(170)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	store.mu.Lock()
	store.saveErr = errors.New("disk full")
	store.mu.Unlock()
	close(release)
	waitFor(t, "unpersistable failure", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusFailed && v.Error != ""
	})
	v, _ := m.Get(id)
	if v.Error != "Unable to persist the job state. Check server storage." {
		t.Errorf("error = %q", v.Error)
	}
	// The job must remain deletable even though saving is broken.
	store.mu.Lock()
	store.saveErr = nil
	store.mu.Unlock()
	if err := m.Delete(id); err != nil {
		t.Fatalf("delete of unpersistable job: %v", err)
	}
}

func TestListSelfHealsMissingReadyFile(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, silentDownloader(), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(180)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	// The downloader succeeds but publishes nothing (no file registered).
	waitFor(t, "ready in memory", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	views, total, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != StatusFailed {
		t.Fatalf("self-heal did not downgrade: %+v", views)
	}
	if views[0].Error == "" {
		t.Error("downgrade lacks an error message")
	}
	if total != 0 {
		t.Errorf("total = %d; want 0", total)
	}
	// The downgrade persists for the next poll too.
	views, _, _ = m.List()
	if views[0].Status != StatusFailed {
		t.Errorf("downgrade not stable: %+v", views[0])
	}
}

func TestListNewestFirstWithTotal(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, silentDownloader(), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	ids := []string{videoID(190), videoID(191), videoID(192)}
	for _, id := range ids {
		if _, _, err := m.Submit(id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "ready", func() bool {
			v, _ := m.Get(id)
			return v.Status == StatusReady
		})
		store.setFile(id, []byte("data"))
	}
	views, total, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 {
		t.Fatalf("views = %d; want 3", len(views))
	}
	if views[0].ID != ids[2] || views[1].ID != ids[1] || views[2].ID != ids[0] {
		t.Errorf("order = %s %s %s; want newest first", views[0].ID, views[1].ID, views[2].ID)
	}
	if total != 12 { // 3 × 4 bytes
		t.Errorf("total = %d; want 12", total)
	}
}

func TestListTieBreaksByID(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, blockedDownload(nil, make(chan struct{}), nil), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()
	small := "aaaaaaaaaaa"
	large := "zzzzzzzzzzz"
	if _, _, err := m.Submit(small); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Submit(large); err != nil {
		t.Fatal(err)
	}
	views, _, _ := m.List()
	if len(views) != 2 || views[0].ID != large || views[1].ID != small {
		t.Errorf("tie-break order = %+v", views)
	}
}

func TestRestartRecovery(t *testing.T) {
	root := t.TempDir()
	store := DiskStore{Root: root}
	now := time.Now().UTC().Truncate(time.Second)

	// A ready job whose video is still present.
	readyID := videoID(200)
	if err := store.Save(Job{ID: readyID, Title: "Ready", Status: StatusReady, SizeBytes: 5000, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	writeVideoFile(t, root, readyID, 5000)
	// A queued job.
	queuedID := videoID(201)
	if err := store.Save(Job{ID: queuedID, Title: "Queued", Status: StatusQueued, CreatedAt: now.Add(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	// A job that was mid-download when the server stopped.
	activeID := videoID(202)
	if err := store.Save(Job{ID: activeID, Title: "Interrupted", Status: StatusDownloading, CreatedAt: now.Add(2 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	// Leftover scratch from the interrupted run.
	work := filepath.Join(root, "videos", activeID, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "partial.mp4"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	CleanWorkFiles(root)

	completed := make(chan struct{}, 3)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		completed <- struct{}{}
		data := []byte("done")
		os.MkdirAll(filepath.Join(root, "videos", j.ID), 0o700)
		os.WriteFile(filepath.Join(root, "videos", j.ID, "video.mp4"), data, 0o600)
		return DownloadResult{Title: j.Title, SizeBytes: int64(len(data))}, nil
	})
	m := NewManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{}, errors.New("no fetch")
	}))
	defer m.Close()
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}

	// Ready stays ready with its original size; the queued job downloads
	// again; the interrupted job becomes retryable, not restarted.
	v, err := m.Get(readyID)
	if err != nil || v.Status != StatusReady || v.SizeBytes != 5000 {
		t.Errorf("ready recovery = %+v, %v", v, err)
	}
	waitFor(t, "queued job to resume", func() bool {
		v, _ := m.Get(queuedID)
		return v.Status == StatusReady
	})
	v, err = m.Get(activeID)
	if err != nil || v.Status != StatusFailed || v.Error == "" {
		t.Errorf("interrupted recovery = %+v, %v", v, err)
	}
	// The retry hint job downloads fine when retried.
	if _, err := m.Retry(activeID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retried job to finish", func() bool {
		v, _ := m.Get(activeID)
		return v.Status == StatusReady
	})
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("leftover work dir survived: %v", err)
	}
	_, total, _ := m.List()
	if total != 5008 {
		t.Errorf("total = %d; want 5008", total)
	}
}

func TestSubmitDuringDeletionWaits(t *testing.T) {
	store := newMemoryStore()
	entered := make(chan struct{}, 1)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		// Wind down slowly so the deletion window is observable.
		time.Sleep(300 * time.Millisecond)
		return DownloadResult{}, ctx.Err()
	})
	m := newTestManager(store, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(210)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	// Let Delete mark the job before racing a resubmit against it.
	time.Sleep(100 * time.Millisecond)
	subDone := make(chan error, 1)
	go func() {
		_, _, err := m.Submit(id)
		subDone <- err
	}()
	select {
	case <-subDone:
		t.Fatal("submit returned while deletion was still in flight")
	case <-time.After(150 * time.Millisecond):
		// Still blocked: the downloader is mid wind-down.
	}
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed")
	}
	if err := <-subDone; err != nil {
		t.Fatalf("submit after in-flight deletion: %v", err)
	}
	if _, err := m.Get(id); err != nil {
		t.Errorf("new job missing after resubmit: %v", err)
	}
}

// A metadata write that is already in flight when Delete arrives must not
// resurrect the job directory after the removal: the finalizer holds the
// job's save lock for the whole removal, so the write either lands before
// it (and is removed too) or is skipped. The gate pins the write mid-flight
// while Delete runs, which on the broken path would recreate the directory.
func TestSaveCannotResurrectDeletedJob(t *testing.T) {
	root := t.TempDir()
	idA, id := videoID(230), videoID(231)
	gate := newSaveGate(DiskStore{Root: root}, func(j Job) bool { return j.ID == id })
	releaseA := make(chan struct{})
	enteredA := make(chan struct{}, 1)
	// The victim fails once through the title pool (the worker never gets
	// past the occupier), so it can be requeued — the requeue's asynchronous
	// save is the write that must not resurrect the removed directory.
	var failVictim atomic.Bool
	failVictim.Store(true)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		enteredA <- struct{}{}
		select {
		case <-releaseA:
			return DownloadResult{Title: "A", SizeBytes: 1}, nil
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		}
	})
	m := newTestManager(gate, dl, fetchFunc(func(ctx context.Context, vid string) (Metadata, error) {
		if vid == id && failVictim.CompareAndSwap(true, false) {
			return Metadata{}, DownloadError("The video is private.")
		}
		return Metadata{Title: "T"}, nil
	}))
	defer m.Close()
	defer close(releaseA)

	if _, _, err := m.Submit(idA); err != nil {
		t.Fatal(err)
	}
	<-enteredA // the worker is busy, so the victim never downloads
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "victim failed by the title pool", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		js := m.jobs[id]
		return js != nil && js.Status == StatusFailed && !js.active && !js.fetching
	})

	gate.arm()
	if _, err := m.Retry(id); err != nil {
		t.Fatal(err)
	}
	<-gate.entered // a save for the victim is now parked inside the gate

	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	select {
	case err := <-delDone:
		t.Fatalf("Delete returned while a metadata save was still in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(gate.release) // the parked save lands — after this, removal is safe
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed after the save landed")
	}
	if _, err := os.Stat(filepath.Join(root, "videos", id)); !os.IsNotExist(err) {
		t.Errorf("job directory survived: %v", err)
	}
	// The proof against resurrection: a write that landed after Remove
	// would have recreated metadata.json and the card would return on the
	// next restart.
	jobs, err := gate.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != idA { // only the occupier remains
		t.Errorf("scan after delete = %+v", jobs)
	}
}

// Delete must not finish while a metadata save holds the job's save lock,
// even on the worker path: the finalizer queues behind the save, so the
// removal always sees a quiet directory. The store afterwards must not
// contain the job — a save landing after Remove would have re-inserted it.
func TestDeletionWaitsForInFlightSave(t *testing.T) {
	inner := newMemoryStore()
	idA, id := videoID(232), videoID(233)
	gate := newSaveGate(inner, func(j Job) bool { return j.ID == id && j.Status == StatusDownloading })
	gate.arm() // only the victim's "downloading" write is ever held
	releaseA := make(chan struct{})
	enteredA := make(chan struct{}, 1)
	dl := downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		if j.ID == idA {
			enteredA <- struct{}{}
			select {
			case <-releaseA:
				return DownloadResult{Title: "A", SizeBytes: 1}, nil
			case <-ctx.Done():
				return DownloadResult{}, ctx.Err()
			}
		}
		<-ctx.Done()
		return DownloadResult{}, ctx.Err()
	})
	m := newTestManager(gate, dl, fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "T"}, nil
	}))
	defer m.Close()

	if _, _, err := m.Submit(idA); err != nil {
		t.Fatal(err)
	}
	<-enteredA
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	// Let the title pool finish with the victim before it downloads, so the
	// only gated save is the pipeline's "downloading" write.
	waitFor(t, "title applied and fetcher released", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		js := m.jobs[id]
		return js != nil && js.Title == "T" && !js.fetching
	})
	close(releaseA) // the worker moves on to the victim; its first save parks
	<-gate.entered

	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	select {
	case err := <-delDone:
		t.Fatalf("Delete returned while a metadata save was still in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(gate.release)
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed after the save landed")
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v; want ErrNotFound", err)
	}
	if _, err := inner.Load(id); err != ErrNotFound {
		t.Errorf("store kept the deleted job: %v", err)
	}
}

// A removal failure must reach the API caller, keep the card (as failed for
// unfinished jobs), and stay retryable once the storage recovers.
func TestDeleteRemovalFailurePropagatesAndKeepsCard(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	m := newTestManager(store, blockedDownload(nil, release, func() { entered <- struct{}{} }), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(234)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	store.mu.Lock()
	store.removeErr = errors.New("permission denied")
	store.mu.Unlock()

	if err := m.Delete(id); err != ErrStorage {
		t.Fatalf("delete with broken storage = %v; want ErrStorage", err)
	}
	v, err := m.Get(id)
	if err != nil {
		t.Fatalf("card vanished after failed delete: %v", err)
	}
	if v.Status != StatusFailed {
		t.Errorf("status after failed delete = %q; want failed", v.Status)
	}
	if v.Error != "Unable to delete the video files. Check server storage and try again." {
		t.Errorf("error = %q", v.Error)
	}
	// The failed card is still there for a second attempt once the store heals.
	store.mu.Lock()
	store.removeErr = nil
	store.mu.Unlock()
	waitFor(t, "workers released", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		js := m.jobs[id]
		return js != nil && !js.active && !js.fetching
	})
	if err := m.Delete(id); err != nil {
		t.Fatalf("retry of failed delete: %v", err)
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after retried delete = %v; want ErrNotFound", err)
	}
}

// The direct delete path shares the worker path's failure semantics: a
// ready card stays ready, the caller sees the storage error, and the delete
// is retryable.
func TestDeleteReadyRemovalFailureDirectPath(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, silentDownloader(), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(235)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ready", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusReady
	})
	store.mu.Lock()
	store.removeErr = errors.New("permission denied")
	store.mu.Unlock()
	if err := m.Delete(id); err != ErrStorage {
		t.Fatalf("delete with broken storage = %v; want ErrStorage", err)
	}
	if v, err := m.Get(id); err != nil || v.Status != StatusReady {
		t.Errorf("ready card after failed delete = %+v, %v", v, err)
	}
	store.mu.Lock()
	store.removeErr = nil
	store.mu.Unlock()
	if err := m.Delete(id); err != nil {
		t.Fatalf("retry of failed delete: %v", err)
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after retried delete = %v; want ErrNotFound", err)
	}
}

// Files that are already gone are a successful delete, not an error.
func TestDeleteMissingFilesSucceeds(t *testing.T) {
	root := t.TempDir()
	store := DiskStore{Root: root}
	m := newTestManager(store, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		dir := filepath.Join(root, "videos", j.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return DownloadResult{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("data"), 0o600); err != nil {
			return DownloadResult{}, err
		}
		return DownloadResult{Title: "Ready", SizeBytes: 4}, nil
	}), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		return Metadata{Title: "x"}, nil
	}))
	defer m.Close()

	id := videoID(236)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	// Wait for the persisted record, not just the in-memory view: the
	// ready-state save must have landed before the directory is removed.
	waitFor(t, "ready state persisted", func() bool {
		j, err := store.Load(id)
		return err == nil && j.Status == StatusReady
	})
	// The directory disappears out from under the registry (an operator
	// cleaned it up); Delete must still complete the removal.
	if err := os.RemoveAll(filepath.Join(root, "videos", id)); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(id); err != nil {
		t.Fatalf("delete of already-missing files: %v", err)
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v; want ErrNotFound", err)
	}
}

// While both the download and a title fetch still hold the job, Delete waits
// for the last owner: the download winding down alone must not complete the
// removal out from under the still-running fetcher.
func TestDeleteDuringDownloadAndTitleFetch(t *testing.T) {
	store := newMemoryStore()
	fetchRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseFetcher := func() { releaseOnce.Do(func() { close(fetchRelease) }) }
	entered := make(chan struct{}, 1)
	m := newTestManager(store, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		entered <- struct{}{}
		<-ctx.Done() // stops when Delete cancels, like a real pipeline
		return DownloadResult{}, ctx.Err()
	}), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		// The fetcher outlives the download's cancellation; only the test
		// releases it, so the deletion window stays observable.
		<-fetchRelease
		return Metadata{}, errors.New("offline")
	}))
	defer m.Close()
	defer releaseFetcher()

	id := videoID(237)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	<-entered
	waitFor(t, "fetch to start", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.jobs[id] != nil && m.jobs[id].fetching
	})

	delDone := make(chan error, 1)
	go func() { delDone <- m.Delete(id) }()
	// Delete cancels the pipeline; the fetcher still owns the job, so the
	// removal must not complete until it stops too.
	select {
	case err := <-delDone:
		t.Fatalf("Delete returned while the title fetcher was still running: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	releaseFetcher()
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatalf("Delete failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed after the fetcher stopped")
	}
	if _, err := m.Get(id); err != ErrNotFound {
		t.Errorf("Get after delete = %v; want ErrNotFound", err)
	}
}

// Retries and resubmissions count toward the admission limit exactly like
// fresh submissions: with the queue full, both return ErrBusy and leave the
// failed card untouched.
func TestRetryAndResubmitRespectAdmissionLimit(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	signalEntered := func() {
		select {
		case entered <- struct{}{}:
		default: // only the occupier's entry is awaited
		}
	}
	liveID := videoID(238)
	// Only the first metadata fetch rejects the video: the fresh submission
	// must fail fast, but the retried one must complete — otherwise the title
	// pool races the download worker and re-fails the job before it can
	// finish, making the final wait flaky.
	var liveFailedOnce atomic.Bool
	m := newTestManager(store, blockedDownload(nil, release, signalEntered), fetchFunc(func(_ context.Context, id string) (Metadata, error) {
		if id == liveID && !liveFailedOnce.Swap(true) {
			return Metadata{}, DownloadError("Live and upcoming streams are not supported.")
		}
		return Metadata{Title: "T"}, nil
	}))
	defer m.Close()

	if _, _, err := m.Submit(videoID(239)); err != nil {
		t.Fatal(err)
	}
	<-entered // worker busy; every later submission stays queued
	// The failed card must exist before the queue fills, or its own fresh
	// submission is rejected by the admission limit being tested.
	if _, _, err := m.Submit(liveID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "live job to fail", func() bool {
		v, _ := m.Get(liveID)
		return v.Status == StatusFailed
	})
	for i := 0; i < maxUnfinishedJobs-1; i++ {
		if _, _, err := m.Submit(videoID(byte(240 + i))); err != nil {
			t.Fatal(err)
		}
	}
	// Sixteen unfinished jobs plus the failed card that wants back in.

	if _, err := m.Retry(liveID); err != ErrBusy {
		t.Errorf("retry at full queue = %v; want ErrBusy", err)
	}
	if _, _, err := m.Submit(liveID); err != ErrBusy {
		t.Errorf("resubmit at full queue = %v; want ErrBusy", err)
	}
	if v, err := m.Get(liveID); err != nil || v.Status != StatusFailed {
		t.Errorf("failed card touched by rejected requeue: %+v, %v", v, err)
	}

	close(release)
	waitFor(t, "queue to drain", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.unfinishedLocked() == 0
	})
	if _, err := m.Retry(liveID); err != nil {
		t.Fatalf("retry after drain: %v", err)
	}
	waitFor(t, "retried job to finish", func() bool {
		v, _ := m.Get(liveID)
		return v.Status == StatusReady
	})
}

func TestViewDefaultTitleWhileQueued(t *testing.T) {
	store := newMemoryStore()
	m := newTestManager(store, blockedDownload(nil, make(chan struct{}), nil), fetchFunc(func(ctx context.Context, id string) (Metadata, error) {
		<-ctx.Done()
		return Metadata{}, ctx.Err()
	}))
	defer m.Close()

	id := videoID(220)
	if _, _, err := m.Submit(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "downloading", func() bool {
		v, _ := m.Get(id)
		return v.Status == StatusDownloading
	})
	v, _ := m.Get(id)
	if v.Title != "Fetching title…" {
		t.Errorf("placeholder title = %q", v.Title)
	}
}

func TestPublicErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrStorage, ErrStorage.Error()},
		{context.Canceled, "The download was interrupted. Tap Retry to try again."},
		{context.DeadlineExceeded, "The download timed out. Tap Retry to try again."},
		{DownloadError("Safe message."), "Safe message."},
		{fmt.Errorf("exec yt-dlp: /private/var/tmp/xyz"), "Unable to download this video. It may be unavailable, private, or age-restricted."},
		{fmt.Errorf("wrapped: %w", context.Canceled), "The download was interrupted. Tap Retry to try again."},
		{fmt.Errorf("wrapped: %w", DownloadError("Safe.")), "Safe."},
	}
	for _, c := range cases {
		if got := publicError(c.err); got != c.want {
			t.Errorf("publicError(%v) = %q; want %q", c.err, got, c.want)
		}
	}
}
