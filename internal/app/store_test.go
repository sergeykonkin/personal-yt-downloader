package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDiskStore(t *testing.T) (DiskStore, string) {
	t.Helper()
	root := t.TempDir()
	return DiskStore{Root: root}, root
}

func writeJob(t *testing.T, s DiskStore, j Job) {
	t.Helper()
	if err := s.Save(j); err != nil {
		t.Fatalf("Save(%s): %v", j.ID, err)
	}
}

func writeVideoFile(t *testing.T, root, id string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "videos", id), 0o700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(root, "videos", id, "video.mp4"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiskStoreSaveLoadRemove(t *testing.T) {
	s, root := newDiskStore(t)
	created := time.Now().UTC().Truncate(time.Second)
	j := Job{ID: videoID(1), Title: "A video", Status: StatusReady, SizeBytes: 42, CreatedAt: created, DurationSec: 61.5}
	writeJob(t, s, j)
	writeVideoFile(t, root, j.ID, 42) // a ready job must have its file

	loaded, err := s.Load(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != j.Title || loaded.Status != j.Status || loaded.SizeBytes != 42 ||
		!loaded.CreatedAt.Equal(created) || loaded.DurationSec != 61.5 {
		t.Errorf("roundtrip mismatch: %+v", loaded)
	}
	if _, err := s.Load("missingid123"); err != ErrNotFound {
		t.Errorf("Load(missing) = %v; want ErrNotFound", err)
	}
	if _, err := s.Load("bad id!"); err != ErrNotFound {
		t.Errorf("Load(bad id) = %v; want ErrNotFound", err)
	}

	if err := s.Remove(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(j.ID); err != ErrNotFound {
		t.Errorf("Load after Remove = %v; want ErrNotFound", err)
	}
	if err := s.Remove(j.ID); err == nil {
		t.Error("removing twice succeeded")
	}
}

func TestDiskStoreAtomicWriteLeavesNoTempFiles(t *testing.T) {
	s, root := newDiskStore(t)
	writeJob(t, s, Job{ID: videoID(2), Status: StatusQueued, CreatedAt: time.Now().UTC()})
	entries, err := os.ReadDir(filepath.Join(root, "videos", videoID(2)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "metadata.json" {
		t.Errorf("job dir contains leftovers: %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("metadata permissions = %v; want 0600", info.Mode().Perm())
	}
}

func TestDiskStoreScan(t *testing.T) {
	s, root := newDiskStore(t)
	idA, idB, idC := videoID(3), videoID(4), videoID(5)
	writeJob(t, s, Job{ID: idA, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeJob(t, s, Job{ID: idB, Status: StatusQueued, CreatedAt: time.Now().UTC()})
	// idC: a directory with no metadata — stays visible and deletable.
	if err := os.MkdirAll(filepath.Join(root, "videos", idC), 0o700); err != nil {
		t.Fatal(err)
	}
	// Ignored: non-job entries.
	if err := os.MkdirAll(filepath.Join(root, "videos", "stray"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "videos", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	jobs, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if len(jobs) != 3 {
		t.Fatalf("Scan returned %d jobs; want 3: %+v", len(jobs), jobs)
	}
	if byID[idC].Status != StatusFailed || byID[idC].Title != "" {
		t.Errorf("orphan dir not surfaced as failed: %+v", byID[idC])
	}
	if byID[idA].Status != StatusReady {
		t.Errorf("ready job lost status: %+v", byID[idA])
	}
}

func TestDiskStoreScanMissingVideosDir(t *testing.T) {
	s, _ := newDiskStore(t)
	jobs, err := s.Scan()
	if err != nil || len(jobs) != 0 {
		t.Errorf("Scan on empty root = %v, %v", jobs, err)
	}
}

func TestDecodeJobFallbacks(t *testing.T) {
	id := videoID(6)
	now := time.Now().UTC()
	cases := []struct {
		name string
		data string
	}{
		{"not json", "{"},
		{"mismatched id", `{"id":"other","status":"queued","created_at":"` + now.Format(time.RFC3339Nano) + `"}`},
		{"zero created_at", `{"id":"` + id + `","status":"queued","created_at":"0001-01-01T00:00:00Z"}`},
		{"unknown status", `{"id":"` + id + `","status":"weird","created_at":"` + now.Format(time.RFC3339Nano) + `"}`},
	}
	for _, c := range cases {
		j, valid := decodeJob([]byte(c.data), id)
		if valid || j.Status != StatusFailed || j.ID != id || j.Error == "" {
			t.Errorf("%s: decodeJob = %+v, valid=%v; want synthesized failed job", c.name, j, valid)
		}
	}
	// A valid record decodes cleanly.
	good := `{"id":"` + id + `","status":"ready","created_at":"` + now.Format(time.RFC3339Nano) + `","title":"ok"}`
	if j, valid := decodeJob([]byte(good), id); !valid || j.Status != StatusReady || j.Title != "ok" {
		t.Errorf("valid record rejected: %+v valid=%v", j, valid)
	}
}

func TestDiskStoreLoadMissingVideoDowngradesReady(t *testing.T) {
	s, _ := newDiskStore(t)
	id := videoID(7)
	created := time.Now().UTC().Truncate(time.Second)
	writeJob(t, s, Job{ID: id, Title: "gone", Status: StatusReady, CreatedAt: created})
	// No video.mp4 on disk.
	j, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusFailed || j.Title != "gone" || !j.CreatedAt.Equal(created) {
		t.Errorf("ready-without-file = %+v", j)
	}
	// An empty video file is just as missing.
	writeVideoFile(t, s.Root, id, 0)
	// Truncate to zero via a fresh write.
	if err := os.Truncate(filepath.Join(s.Root, "videos", id, "video.mp4"), 0); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Load(id); err != nil || j.Status != StatusFailed {
		t.Errorf("ready-with-empty-file = %+v, %v", j, err)
	}
}

func TestDiskStoreVideoSizeAndTotalExcludeMetadataAndTemp(t *testing.T) {
	s, root := newDiskStore(t)
	idA, idB := videoID(8), videoID(9)
	writeJob(t, s, Job{ID: idA, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeJob(t, s, Job{ID: idB, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeVideoFile(t, root, idA, 1000)
	writeVideoFile(t, root, idB, 250)
	// Scratch and metadata must not count toward the total.
	work := filepath.Join(root, "videos", idA, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "partial.mp4"), make([]byte, 999999), 0o600); err != nil {
		t.Fatal(err)
	}

	if size, err := s.VideoSize(idA); err != nil || size != 1000 {
		t.Errorf("VideoSize = %d, %v; want 1000", size, err)
	}
	total, err := s.Total()
	if err != nil || total != 1250 {
		t.Errorf("Total = %d, %v; want 1250", total, err)
	}

	// A queued job's stray files never count.
	idC := videoID(10)
	writeJob(t, s, Job{ID: idC, Status: StatusQueued, CreatedAt: time.Now().UTC()})
	writeVideoFile(t, root, idC, 7777)
	if total, _ := s.Total(); total != 1250 {
		t.Errorf("Total including non-ready job = %d; want 1250", total)
	}
	if _, err := s.VideoSize("missingid123"); err == nil {
		t.Error("VideoSize(missing) succeeded")
	}
}

func TestDiskStoreOpenVideoRejectsMissingAndEmpty(t *testing.T) {
	s, root := newDiskStore(t)
	id := videoID(11)
	writeJob(t, s, Job{ID: id, Status: StatusReady, CreatedAt: time.Now().UTC()})
	if _, _, err := s.OpenVideo(id); err == nil {
		t.Error("OpenVideo without file succeeded")
	}
	writeVideoFile(t, root, id, 64)
	f, mod, err := s.OpenVideo(id)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 8)
	if _, err := f.Read(buf); err != nil {
		t.Error("read failed")
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Error("seek failed")
	}
	if mod.IsZero() {
		t.Error("zero modtime")
	}
	if _, _, err := s.OpenVideo("bad id!"); err != ErrNotFound {
		t.Errorf("OpenVideo(bad id) = %v", err)
	}
	// Directories are not videos.
	if err := os.MkdirAll(filepath.Join(root, "videos", videoID(12), "video.mp4"), 0o700); err == nil {
		if _, _, err := s.OpenVideo(videoID(12)); err == nil {
			t.Error("OpenVideo on a directory succeeded")
		}
	}
}

func TestDiskStoreSaveFailsWhenMetadataPathIsBlocked(t *testing.T) {
	s, root := newDiskStore(t)
	id := videoID(13)
	// A directory occupying the metadata.json path makes the atomic rename
	// fail — simulating an unwritable/Exhausted filesystem without killing
	// earlier writes.
	dir := filepath.Join(root, "videos", id)
	if err := os.MkdirAll(filepath.Join(dir, "metadata.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Job{ID: id, Status: StatusQueued, CreatedAt: time.Now().UTC()}); err == nil {
		t.Error("Save over blocked path succeeded")
	}
}

func TestDiskStoreRemoveKeepsSiblings(t *testing.T) {
	s, root := newDiskStore(t)
	idA, idB := videoID(14), videoID(15)
	writeJob(t, s, Job{ID: idA, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeJob(t, s, Job{ID: idB, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeVideoFile(t, root, idA, 10)
	if err := s.Remove(idA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(idB); err != nil {
		t.Errorf("removing one job damaged another: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "videos"))
	if len(entries) != 1 {
		t.Errorf("videos dir = %v", entries)
	}
}

func TestCleanWorkFiles(t *testing.T) {
	s, root := newDiskStore(t)
	id := videoID(16)
	writeJob(t, s, Job{ID: id, Status: StatusReady, CreatedAt: time.Now().UTC()})
	writeVideoFile(t, root, id, 30)
	work := filepath.Join(root, "videos", id, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "junk.bin"), []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	CleanWorkFiles(root)
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("work dir survived: %v", err)
	}
	// The published video and metadata must be untouched.
	if _, err := os.Stat(filepath.Join(root, "videos", id, "video.mp4")); err != nil {
		t.Error("published video removed")
	}
	if _, err := os.Stat(filepath.Join(root, "videos", id, "metadata.json")); err != nil {
		t.Error("metadata removed")
	}
}
