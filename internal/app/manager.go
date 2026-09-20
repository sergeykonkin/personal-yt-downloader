package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	maxUnfinishedJobs = 16
	downloadWorkers   = 1 // one download/processing job at a time
	titleWorkers      = 2 // metadata retrieval runs independently of downloads
	titleAttempts     = 3
	deleteWait        = 2 * time.Minute
	titleRetryDelay   = 2 * time.Second
)

// jobState is the in-memory, mutex-guarded record for one video. Persistent
// fields live in the embedded Job; progress and stage are volatile.
type jobState struct {
	Job
	seq        uint64
	ctx        context.Context
	cancel     context.CancelFunc
	stage      string
	progress   float64
	known      bool
	active     bool // owned by the download worker
	fetching   bool // owned by a title worker
	deleting   bool
	titleTries int
	removal    *removalAttempt // non-nil exactly while deleting
	saveMu     sync.Mutex      // serializes metadata.json writes for this job
}

// removalAttempt tracks one in-flight removal of a job. err is written
// before done closes, so a waiter can read it right after <-done without
// any further locking.
type removalAttempt struct {
	done chan struct{}
	err  error
}

// Manager owns the job registry, the single download worker, the title
// worker pool, deletion coordination, and restart recovery.
type Manager struct {
	mu         sync.Mutex
	cond       *sync.Cond
	store      Store
	downloader Downloader
	fetcher    MetadataFetcher
	jobs       map[string]*jobState
	seq        uint64
	workers    sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	closing    bool
	now        func() time.Time
}

func NewManager(store Store, downloader Downloader, fetcher MetadataFetcher) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		store: store, downloader: downloader, fetcher: fetcher,
		jobs: map[string]*jobState{}, ctx: ctx, cancel: cancel, now: time.Now,
	}
	m.cond = sync.NewCond(&m.mu)
	for i := 0; i < downloadWorkers; i++ {
		m.workers.Add(1)
		go m.downloadWorker()
	}
	for i := 0; i < titleWorkers; i++ {
		m.workers.Add(1)
		go m.titleWorker()
	}
	return m
}

// Recover rebuilds the registry from persistent state after a restart:
// completed videos stay ready, queued jobs resume, an interrupted active job
// becomes retryable, and leftover scratch files are removed.
func (m *Manager) Recover() error {
	jobs, err := m.store.Scan()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range jobs {
		js := &jobState{Job: j}
		switch j.Status {
		case StatusDownloading, StatusProcessing, StatusVerifying:
			js.Status, js.Error = StatusFailed, "The download was interrupted by a restart. Tap Retry to continue."
		case StatusReady:
			if size, err := m.store.VideoSize(j.ID); err != nil || size == 0 {
				js.Status, js.Error = StatusFailed, "The stored file is missing. Retry to download it again."
			} else {
				js.SizeBytes = size
			}
		}
		js.stage = js.Status
		if js.Status == StatusQueued {
			js.stage = StageQueued
		}
		js.ctx, js.cancel = context.WithCancel(m.ctx)
		js.seq = m.nextSeqLocked()
		m.jobs[j.ID] = js
	}
	m.cond.Broadcast()
	return nil
}

// Submit creates a job or returns the existing one. The bool reports whether
// the video already had a card, so the client can highlight it instead of
// adding a duplicate. A failed job is requeued; ready and in-flight jobs are
// returned as-is.
func (m *Manager) Submit(id string) (jobView, bool, error) {
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return jobView{}, false, ErrClosed
		}
		if js := m.jobs[id]; js != nil {
			if js.deleting {
				// A previous copy of this video is mid-deletion; the map
				// entry must not be replaced underneath its remover, or the
				// pending Delete would never be confirmed. Wait it out — a
				// failed removal closes the attempt too, so this wakes in
				// every case.
				att := js.removal
				m.mu.Unlock()
				select {
				case <-att.done:
				case <-time.After(deleteWait):
					return jobView{}, false, ErrConflict
				}
				continue
			}
			switch js.Status {
			case StatusReady, StatusQueued, StatusDownloading, StatusProcessing, StatusVerifying:
				v := m.viewLocked(js)
				m.mu.Unlock()
				return v, true, nil
			case StatusFailed:
				if err := m.requeueLocked(js); err != nil {
					m.mu.Unlock()
					return jobView{}, false, err
				}
				v := m.viewLocked(js)
				m.mu.Unlock()
				return v, true, nil
			}
		}
		if m.unfinishedLocked() >= maxUnfinishedJobs {
			slog.Warn("Job rejected", "id", id, "reason", "queue_full", "unfinished", m.unfinishedLocked())
			m.mu.Unlock()
			return jobView{}, false, ErrBusy
		}
		js := &jobState{Job: Job{ID: id, Status: StatusQueued, CreatedAt: m.now().UTC()}, seq: m.nextSeqLocked()}
		js.ctx, js.cancel = context.WithCancel(m.ctx)
		js.stage = StageQueued
		if err := m.store.Save(js.Job); err != nil {
			js.cancel()
			m.mu.Unlock()
			return jobView{}, false, ErrStorage
		}
		m.jobs[id] = js
		m.cond.Broadcast()
		slog.Info("Job queued", "id", id, "unfinished", m.unfinishedLocked())
		v := m.viewLocked(js)
		m.mu.Unlock()
		return v, false, nil
	}
}

// Retry requeues a failed job.
func (m *Manager) Retry(id string) (jobView, error) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return jobView{}, ErrClosed
	}
	js := m.jobs[id]
	if js == nil || js.deleting {
		m.mu.Unlock()
		return jobView{}, ErrNotFound
	}
	if js.Status != StatusFailed {
		m.mu.Unlock()
		return jobView{}, ErrConflict
	}
	if err := m.requeueLocked(js); err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}
	v := m.viewLocked(js)
	m.mu.Unlock()
	return v, nil
}

// requeueLocked restarts a failed job, honoring the admission limit: a
// retry or a resubmit counts toward the unfinished cap exactly like a fresh
// submission. The candidate itself is failed, so it is not part of the count.
func (m *Manager) requeueLocked(js *jobState) error {
	if m.unfinishedLocked() >= maxUnfinishedJobs {
		slog.Warn("Requeue rejected", "id", js.ID, "reason", "queue_full", "unfinished", m.unfinishedLocked())
		return ErrBusy
	}
	js.Status, js.stage, js.Error = StatusQueued, StageQueued, ""
	js.progress, js.known, js.SizeBytes = 0, false, 0
	js.seq = m.nextSeqLocked() // requeueing sends the job to the back of the queue
	if js.cancel != nil {
		js.cancel()
	}
	js.ctx, js.cancel = context.WithCancel(m.ctx)
	go m.saveCurrent(js)
	m.cond.Broadcast()
	return nil
}

func (m *Manager) unfinishedLocked() int {
	n := 0
	for _, js := range m.jobs {
		if !js.deleting && js.unfinished() {
			n++
		}
	}
	return n
}

func (m *Manager) nextSeqLocked() uint64 {
	m.seq++
	return m.seq
}

// Get serves a single job view. Jobs being deleted stay visible until their
// removal is confirmed, so a card never disappears early.
func (m *Manager) Get(id string) (jobView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	js := m.jobs[id]
	if js == nil {
		return jobView{}, ErrNotFound
	}
	return m.viewLocked(js), nil
}

// List returns all jobs newest-first plus the total size of the completed
// videos still present. Ready jobs whose file vanished are downgraded
// before views are built, so a never-fulfillable "ready" is not shown.
func (m *Manager) List() ([]jobView, int64, error) {
	m.mu.Lock()
	ready := make([]*jobState, 0)
	for _, js := range m.jobs {
		if js.Status == StatusReady {
			ready = append(ready, js)
		}
	}
	m.mu.Unlock()

	var total int64
	for _, js := range ready {
		size, statErr := m.store.VideoSize(js.ID)
		m.mu.Lock()
		if current, ok := m.jobs[js.ID]; ok && current == js && !js.deleting {
			if statErr != nil || size == 0 {
				js.Status, js.Error, js.stage = StatusFailed, "The stored file is missing. Retry to download it again.", StatusFailed
				m.mu.Unlock()
				go m.saveCurrent(js)
			} else {
				js.SizeBytes = size
				total += size
				m.mu.Unlock()
			}
		} else {
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	views := make([]jobView, 0, len(m.jobs))
	for _, js := range m.jobs {
		views = append(views, m.viewLocked(js))
	}
	m.mu.Unlock()
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		}
		return views[i].ID > views[j].ID
	})
	return views, total, nil
}

// Delete cancels any processes for the job, waits for them to stop, removes
// all of its files, and only then reports success. When a worker still owns
// the job, the last worker to release it finalizes the removal; otherwise
// this call does so directly. A removal that cannot delete the files keeps
// the card and reports the failure, so the client can retry the delete.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	js := m.jobs[id]
	if js == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	if js.deleting {
		// A previous attempt is still winding down; report its outcome
		// rather than starting a second one over the same files.
		att := js.removal
		m.mu.Unlock()
		return waitRemoval(att)
	}
	att := &removalAttempt{done: make(chan struct{})}
	js.removal, js.deleting = att, true
	js.cancel()
	wasRunning := js.active || js.fetching
	m.mu.Unlock()

	if !wasRunning {
		// No worker owns the job; finalize right here.
		return m.finalizeDeletion(js)
	}
	return waitRemoval(att)
}

// waitRemoval blocks until the attempt finishes or the patience window ends.
func waitRemoval(att *removalAttempt) error {
	select {
	case <-att.done:
		return att.err
	case <-time.After(deleteWait):
		return errors.New("Deletion did not complete. Try again.")
	}
}

// Close stops accepting work, cancels running jobs, and waits for the
// workers to finish. Interrupted active jobs are persisted as retryable.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	m.closing = true
	m.cancel()
	m.cond.Broadcast()
	m.mu.Unlock()
	m.workers.Wait()

	m.mu.Lock()
	var interrupted []*jobState
	for _, js := range m.jobs {
		if js.deleting {
			continue
		}
		switch js.Status {
		case StatusDownloading, StatusProcessing, StatusVerifying:
			js.Status, js.stage, js.Error = StatusFailed, StatusFailed, "The download was interrupted by a restart. Tap Retry to continue."
			interrupted = append(interrupted, js)
		}
	}
	m.mu.Unlock()
	for _, js := range interrupted {
		_ = m.saveCurrent(js)
	}
}

func (m *Manager) downloadWorker() {
	defer m.workers.Done()
	for {
		m.mu.Lock()
		var js *jobState
		for {
			if m.closing {
				m.mu.Unlock()
				return
			}
			if candidate := m.pickQueuedLocked(); candidate != nil {
				js = candidate
				js.active = true
				break
			}
			m.cond.Wait()
		}
		m.mu.Unlock()

		m.runDownload(js)

		m.mu.Lock()
		js.active = false
		// Mirror of the title worker's rule: only the last owner completes
		// the deletion. While a title fetch is still winding down, the title
		// worker releases the job and finishes; otherwise the pipeline's own
		// completion already did.
		finish := js.deleting && !js.fetching
		m.mu.Unlock()
		m.cond.Broadcast()
		if finish {
			m.finalizeDeletion(js)
		}
	}
}

func (m *Manager) pickQueuedLocked() *jobState {
	var best *jobState
	for _, js := range m.jobs {
		if js.Status != StatusQueued || js.deleting || js.active {
			continue
		}
		if best == nil || js.seq < best.seq {
			best = js
		}
	}
	return best
}

func (m *Manager) runDownload(js *jobState) {
	m.mu.Lock()
	js.Status = StatusDownloading
	// Snapshot the job for the pipeline: a title worker may update the
	// embedded Job while the download runs, and js.ctx may be replaced by a
	// requeue, so neither can be read unlocked.
	job, ctx := js.Job, js.ctx
	m.mu.Unlock()
	_ = m.saveCurrent(js)

	report := func(stage string, progress float64, known bool) {
		m.mu.Lock()
		if current, ok := m.jobs[js.ID]; !ok || current != js {
			m.mu.Unlock()
			return
		}
		js.stage, js.progress, js.known = stage, progress, known
		transition := false
		if status := statusForStage(stage); status != js.Status {
			js.Status = status
			transition = true
		}
		m.mu.Unlock()
		if transition {
			go m.saveCurrent(js)
		}
	}
	result, err := m.downloader.Download(ctx, job, report)

	m.mu.Lock()
	if current, ok := m.jobs[js.ID]; !ok || current != js || js.deleting {
		// The job was deleted (or replaced by a resubmit) while the download
		// ran; the worker epilogue finalizes any pending removal.
		m.mu.Unlock()
		return
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && m.closing {
			// Close() converts interrupted jobs once workers exit.
			m.mu.Unlock()
			return
		}
		js.Status, js.stage, js.Error = StatusFailed, StatusFailed, publicError(err)
		if result.Title != "" && js.Title == "" {
			js.Title = result.Title
		}
		// Capture before unlocking: a retry can requeue and rewrite js.Error
		// the moment the mutex is released.
		msg := js.Error
		m.mu.Unlock()
		if saveErr := m.saveCurrent(js); saveErr != nil {
			m.markUnpersistable(js)
		}
		slog.Error("Job failed", "id", js.ID, "error", msg)
		return
	}
	if result.Title != "" {
		js.Title = result.Title
	}
	if result.DurationSec > 0 {
		js.DurationSec = result.DurationSec
	}
	js.Status, js.stage = StatusReady, StageReady
	js.progress, js.known, js.SizeBytes = 100, true, result.SizeBytes
	js.Error = ""
	m.mu.Unlock()
	if saveErr := m.saveCurrent(js); saveErr != nil {
		m.markUnpersistable(js)
	}
	slog.Info("Job completed", "id", js.ID, "bytes", result.SizeBytes)
}

// finalizeDeletion removes the job's files and registry entry, completing
// exactly one removal attempt: Delete's direct path and both worker
// epilogues funnel here. Holding js.saveMu across the whole
// check→Remove→delete sequence makes it mutually exclusive with saveCurrent,
// so a concurrent metadata write can never resurrect the directory after
// the removal: the write either lands before it (and is removed with
// everything else) or is skipped once deleting is seen. saveMu is always
// taken before mu, never the other way around.
func (m *Manager) finalizeDeletion(js *jobState) error {
	js.saveMu.Lock()
	defer js.saveMu.Unlock()
	m.mu.Lock()
	current, ok := m.jobs[js.ID]
	if !ok || current != js || !js.deleting {
		// Already finalized, or a fresh copy of this video replaced the
		// card after an earlier removal completed.
		m.mu.Unlock()
		return nil
	}
	att := js.removal
	m.mu.Unlock()

	err := m.store.Remove(js.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		// The files could not be removed. Keep the card so the client can
		// retry the delete; an unfinished job turns failed so the card
		// states plainly that its files are stuck.
		att.err = ErrStorage
		m.mu.Lock()
		if current, ok := m.jobs[js.ID]; ok && current == js {
			js.deleting, js.removal = false, nil
			if js.unfinished() {
				js.Status, js.stage = StatusFailed, StatusFailed
				js.Error = "Unable to delete the video files. Check server storage and try again."
				m.mu.Unlock()
				go m.saveCurrent(js) // best-effort: the store may be broken
			} else {
				m.mu.Unlock()
			}
		} else {
			m.mu.Unlock()
		}
		close(att.done)
		slog.Error("Unable to remove job directory", "id", js.ID, "error", err.Error())
		return ErrStorage
	}
	m.mu.Lock()
	if current, ok := m.jobs[js.ID]; ok && current == js {
		delete(m.jobs, js.ID)
	}
	m.mu.Unlock()
	close(att.done)
	slog.Info("Job deleted", "id", js.ID)
	return nil
}

func (m *Manager) titleWorker() {
	defer m.workers.Done()
	for {
		m.mu.Lock()
		var js *jobState
		for {
			if m.closing {
				m.mu.Unlock()
				return
			}
			if candidate := m.pickTitleLocked(); candidate != nil {
				js = candidate
				js.fetching = true
				break
			}
			m.cond.Wait()
		}
		m.mu.Unlock()

		m.runTitleFetch(js)

		m.mu.Lock()
		js.fetching = false
		// Complete the deletion only when the download worker is not still
		// holding the job: it owns the running processes, and its release
		// performs the removal. The finalizer's own checks make a double
		// attempt a no-op.
		finish := js.deleting && !js.active
		m.mu.Unlock()
		m.cond.Broadcast()
		if finish {
			m.finalizeDeletion(js)
		}
	}
}

// pickTitleLocked selects the next job needing metadata. Any unfinished job
// qualifies, not just queued ones: the download worker flips a job to
// "downloading" almost immediately, and the pool's fetch is what surfaces
// the title while the pipeline is still busy fetching the media itself.
func (m *Manager) pickTitleLocked() *jobState {
	var best *jobState
	for _, js := range m.jobs {
		if js.Title != "" || js.fetching || js.deleting || js.titleTries >= titleAttempts || !js.unfinished() {
			continue
		}
		if best == nil || js.seq < best.seq {
			best = js
		}
	}
	return best
}

// runTitleFetch retrieves metadata for a queued job so its title appears
// before the download starts. Unsupported videos fail immediately; network
// errors are retried a bounded number of times and otherwise ignored — the
// download worker retries metadata itself.
func (m *Manager) runTitleFetch(js *jobState) {
	m.mu.Lock()
	ctx, job := js.ctx, js.Job // requeue can replace js.ctx; never read it unlocked
	m.mu.Unlock()
	for attempt := 1; attempt <= titleAttempts; attempt++ {
		meta, err := m.fetcher.Fetch(ctx, job.ID)
		if err == nil {
			m.mu.Lock()
			if current, ok := m.jobs[js.ID]; ok && current == js && !js.deleting && js.Title == "" {
				js.Title = meta.Title
				if meta.DurationSec > 0 {
					js.DurationSec = meta.DurationSec
				}
				m.mu.Unlock()
				if saveErr := m.saveCurrent(js); saveErr != nil {
					m.markUnpersistable(js)
				} else {
					slog.Info("Title fetched", "id", js.ID)
				}
			} else {
				m.mu.Unlock()
			}
			return
		}
		var unsupported DownloadError
		if errors.As(err, &unsupported) {
			m.mu.Lock()
			if current, ok := m.jobs[js.ID]; ok && current == js && !js.deleting && js.Status == StatusQueued {
				js.Status, js.stage, js.Error = StatusFailed, StatusFailed, string(unsupported)
				m.mu.Unlock()
				if saveErr := m.saveCurrent(js); saveErr != nil {
					m.markUnpersistable(js)
				}
				slog.Warn("Job rejected by metadata", "id", js.ID, "reason", "unsupported")
			} else {
				m.mu.Unlock()
			}
			return
		}
		if js.ctx.Err() != nil || m.isClosing() {
			return
		}
		if attempt < titleAttempts {
			select {
			case <-js.ctx.Done():
				return
			case <-time.After(titleRetryDelay):
			}
		}
	}
	m.mu.Lock()
	if current, ok := m.jobs[js.ID]; ok && current == js {
		js.titleTries = titleAttempts
	}
	m.mu.Unlock()
}

func (m *Manager) isClosing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

// saveCurrent writes the job's current persistent fields atomically. The
// snapshot is taken while holding the save lock, so concurrent writers can
// never persist a stale state over a newer one. Deleted jobs are skipped so
// removal is never undone.
func (m *Manager) saveCurrent(js *jobState) error {
	js.saveMu.Lock()
	defer js.saveMu.Unlock()
	m.mu.Lock()
	if current, ok := m.jobs[js.ID]; !ok || current != js || js.deleting {
		m.mu.Unlock()
		return nil
	}
	snapshot := js.Job
	m.mu.Unlock()
	if err := m.store.Save(snapshot); err != nil {
		slog.Error("Unable to persist job state", "id", js.ID, "error", err.Error())
		return err
	}
	return nil
}

// markUnpersistable flags a volatile error when a result cannot be saved.
// Completed videos on disk are never touched.
func (m *Manager) markUnpersistable(js *jobState) {
	m.mu.Lock()
	if current, ok := m.jobs[js.ID]; ok && current == js && !js.deleting {
		js.Status, js.stage, js.Error = StatusFailed, StatusFailed, "Unable to persist the job state. Check server storage."
	}
	m.mu.Unlock()
	slog.Error("Job state unpersistable", "id", js.ID)
}

func (m *Manager) viewLocked(js *jobState) jobView {
	stage := js.stage
	progress, known := js.progress, js.known
	switch js.Status {
	case StatusReady:
		stage, progress, known = StageReady, 100, true
	case StatusFailed:
		stage = StageFailed
	case StatusQueued:
		if stage == "" {
			stage = StageQueued
		}
	}
	return view(js.Job, stage, progress, known)
}

// CleanWorkFiles removes every job's scratch directory. Called at startup,
// any leftover work directory is by definition from an interrupted run.
func CleanWorkFiles(root string) {
	videos := filepath.Join(root, "videos")
	entries, err := os.ReadDir(videos)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			_ = os.RemoveAll(filepath.Join(videos, entry.Name(), "work"))
		}
	}
}
