package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File is an open, seekable media file. Deletion may unlink the underlying
// path while a File is open; delivery continues from the open descriptor.
type File interface {
	io.ReadSeeker
	io.Closer
}

func jsonMarshalStrict(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Store persists job metadata and media files. All mutations are atomic:
// readers observe either the previous state or the new one, never a partial
// write, even across crashes.
type Store interface {
	Save(Job) error
	Load(id string) (Job, error)
	Remove(id string) error
	Scan() ([]Job, error)
	OpenVideo(id string) (File, time.Time, error)
	VideoSize(id string) (int64, error)
	Total() (int64, error)
}

// DiskStore lays out data as:
//
//	<root>/videos/<id>/metadata.json   persistent job state
//	<root>/videos/<id>/video.mp4       published media
//	<root>/videos/<id>/work/           scratch space, removed on completion
type DiskStore struct{ Root string }

func (s DiskStore) videosDir() string          { return filepath.Join(s.Root, "videos") }
func (s DiskStore) dir(id string) string       { return filepath.Join(s.videosDir(), id) }
func (s DiskStore) videoPath(id string) string { return filepath.Join(s.dir(id), "video.mp4") }

func (s DiskStore) Save(j Job) error {
	dir := s.dir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := writeFileSync(dir, "metadata.json", data); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileSync writes a file via a temporary sibling and an atomic rename,
// so a crash mid-write never leaves a truncated file behind.
func writeFileSync(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func (s DiskStore) Load(id string) (Job, error) {
	if !idPattern.MatchString(id) {
		return Job{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.dir(id), "metadata.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j, valid := decodeJob(data, id)
	if !valid {
		return j, nil // synthesized failed job for an unreadable record
	}
	if j.Status == StatusReady {
		info, statErr := os.Stat(s.videoPath(id))
		if errors.Is(statErr, fs.ErrNotExist) || (statErr == nil && (!info.Mode().IsRegular() || info.Size() == 0)) {
			return Job{ID: id, Status: StatusFailed, CreatedAt: j.CreatedAt, Title: j.Title, Error: "The stored file is missing. Retry to download it again."}, nil
		}
		if statErr != nil {
			return Job{}, statErr
		}
	}
	return j, nil
}

// decodeJob parses stored metadata and enforces invariants. The bool reports
// whether the record was valid; invalid records surface as a failed job
// carrying the directory's ID so the entry stays visible and deletable.
func decodeJob(data []byte, id string) (Job, bool) {
	var j Job
	if json.Unmarshal(data, &j) != nil || j.ID != id || j.CreatedAt.IsZero() {
		return Job{ID: id, Status: StatusFailed, CreatedAt: time.Now().UTC(), Error: "The stored job data is unreadable. Delete it and submit the URL again."}, false
	}
	switch j.Status {
	case StatusQueued, StatusDownloading, StatusProcessing, StatusVerifying, StatusReady, StatusFailed:
	default:
		return Job{ID: id, Status: StatusFailed, CreatedAt: j.CreatedAt, Title: j.Title, Error: "The stored job data is unreadable. Delete it and submit the URL again."}, false
	}
	return j, true
}

// Scan loads every job directory for startup recovery.
func (s DiskStore) Scan() ([]Job, error) {
	entries, err := os.ReadDir(s.videosDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(entries))
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !idPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir(id), "metadata.json"))
		if errors.Is(err, fs.ErrNotExist) {
			jobs = append(jobs, Job{ID: id, Status: StatusFailed, CreatedAt: time.Now().UTC(), Error: "The stored job data is unreadable. Delete it and submit the URL again."})
			continue
		}
		if err != nil {
			return nil, err
		}
		j, _ := decodeJob(data, id)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (s DiskStore) Remove(id string) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	if _, err := os.Stat(s.dir(id)); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return err
	}
	// Sync the parent so the removal survives an abrupt power loss.
	d, err := os.Open(s.videosDir())
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s DiskStore) OpenVideo(id string) (File, time.Time, error) {
	if !idPattern.MatchString(id) {
		return nil, time.Time{}, ErrNotFound
	}
	f, err := os.Open(s.videoPath(id))
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		f.Close()
		return nil, time.Time{}, ErrStorage
	}
	return f, info.ModTime(), nil
}

func (s DiskStore) VideoSize(id string) (int64, error) {
	info, err := os.Stat(s.videoPath(id))
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, ErrStorage
	}
	return info.Size(), nil
}

// Total sums the sizes of the published videos of ready jobs, recomputed
// from the filesystem so it reflects exactly what is still present.
func (s DiskStore) Total() (int64, error) {
	jobs, err := s.Scan()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, j := range jobs {
		if j.Status != StatusReady {
			continue
		}
		size, err := s.VideoSize(j.ID)
		if err != nil {
			continue // vanished between Scan and Stat; list() reports it as failed
		}
		total += size
	}
	return total, nil
}
