package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinkIssueAndLookup(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	links := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)

	id := videoID(1)
	created := clk.Now().Add(-time.Hour)
	token, expires, err := links.Issue(id, created)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || !expires.Equal(clk.Now().Add(linkTTL)) {
		t.Fatalf("bad link material: %q %v", token, expires)
	}
	gotID, gotCreated, ok := links.Lookup(token)
	if !ok || gotID != id || !gotCreated.Equal(created) {
		t.Errorf("lookup = %q, %v, %v", gotID, gotCreated, ok)
	}
	if _, _, ok := links.Lookup("forged"); ok {
		t.Error("forged token accepted")
	}
	if _, _, ok := links.Lookup(""); ok {
		t.Error("empty token accepted")
	}
}

func TestLinkExpiry(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	links := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)
	token, _, err := links.Issue(videoID(2), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(linkTTL - time.Minute)
	if _, _, ok := links.Lookup(token); !ok {
		t.Error("link expired too early")
	}
	clk.Advance(2 * time.Minute)
	if _, _, ok := links.Lookup(token); ok {
		t.Error("expired link still valid")
	}
	// Reload drops expired entries from disk too.
	reloaded := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reloaded.Lookup(token); ok {
		t.Error("expired link survived reload")
	}
}

func TestLinkRevocation(t *testing.T) {
	dir := t.TempDir()
	links := NewLinkStore(filepath.Join(dir, "links.json"), time.Now)
	id := videoID(3)
	tokens := make([]string, 3)
	for i := range tokens {
		token, _, err := links.Issue(id, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = token
	}
	// Other videos' links must survive.
	other, _, err := links.Issue(videoID(4), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	links.Revoke(id)
	for _, token := range tokens {
		if _, _, ok := links.Lookup(token); ok {
			t.Error("revoked link still valid")
		}
	}
	if _, _, ok := links.Lookup(other); !ok {
		t.Error("unrelated link revoked")
	}
	// Revoked, expired, and invalid links are indistinguishable.
	if _, _, ok := links.Lookup(tokens[0]); ok {
		t.Error("revoked link distinguishable")
	}
}

func TestLinkCapEvictsOldestPerVideo(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	links := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)
	id := videoID(5)
	var first string
	for i := 0; i < maxLinksPerVideo+2; i++ {
		token, _, err := links.Issue(id, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = token
		}
		clk.Advance(time.Minute)
	}
	if _, _, ok := links.Lookup(first); ok {
		t.Error("oldest link not evicted at cap")
	}
	// The cap is per video, not global.
	if _, _, err := links.Issue(videoID(6), clk.Now()); err != nil {
		t.Errorf("per-video cap leaked globally: %v", err)
	}
}

func TestLinkStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "links.json")
	links := NewLinkStore(path, time.Now)
	id := videoID(7)
	created := time.Now().UTC()
	token, _, err := links.Issue(id, created)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewLinkStore(path, time.Now)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	gotID, gotCreated, ok := reloaded.Lookup(token)
	if !ok || gotID != id || !gotCreated.Equal(created) {
		t.Errorf("link did not survive reload: %q %v %v", gotID, gotCreated, ok)
	}
	// Only the token's hash is stored.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || bytes.Contains(data, []byte(token)) {
		t.Error("plaintext token stored in links.json")
	}
}

func TestLinkStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "links.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := NewLinkStore(path, time.Now)
	if err := links.Load(); err == nil {
		t.Error("corrupt links file loaded without error")
	}
	// A missing file is fine.
	links2 := NewLinkStore(filepath.Join(dir, "absent.json"), time.Now)
	if err := links2.Load(); err != nil {
		t.Errorf("missing links file: %v", err)
	}
}

func TestLinkPrune(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	links := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)
	token, _, _ := links.Issue(videoID(8), clk.Now())
	clk.Advance(linkTTL + time.Hour)
	links.Prune()
	if _, _, ok := links.Lookup(token); ok {
		t.Error("prune left an expired link")
	}
}

func TestLinkIssuePersistFailure(t *testing.T) {
	// The link store cannot persist, so Issue must fail and leave no
	// in-memory entry behind. A file squatting on the directory path makes
	// the save fail for every user, root included (root ignores mode
	// bits).
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := NewLinkStore(filepath.Join(blocked, "links.json"), time.Now)
	if _, _, err := links.Issue(videoID(9), time.Now()); err == nil {
		t.Error("issue succeeded on an unwritable directory")
	}
}
