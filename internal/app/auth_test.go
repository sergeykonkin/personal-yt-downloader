package app

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(encoded, "correct horse battery staple") {
		t.Error("correct password rejected")
	}
	if verifyPassword(encoded, "wrong password") {
		t.Error("wrong password accepted")
	}
	if verifyPassword(encoded, "") {
		t.Error("empty password accepted")
	}
	// Different salts must yield different hashes.
	other, _ := hashPassword("correct horse battery staple")
	if encoded == other {
		t.Error("salt reuse detected")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2id$v=19$m=65536,t=2,p=4$short",
		"$argon2id$v=19$m=65536,t=2,p=4$onlysalt",
		"$argon2id$v=19$m=65536,t=2,p=4$!!invalid!!$hash",
		"$argon2i$v=19$m=65536,t=2,p=4$c2FsdA$c2hhcmt3aGljaGlzYXRvb25n",
		"$argon2id$v=19$m=0,t=2,p=4$c2FsdA$c2hhcmt3aGljaGlzYXRvbmd3aGljaGlzYXRvbmc",
	} {
		if verifyPassword(bad, "anything") {
			t.Errorf("malformed hash accepted: %q", bad)
		}
	}
}

func TestAuthVerifiesEnvPassword(t *testing.T) {
	a := NewAuth("first-password", "")
	if !a.Verify("first-password") {
		t.Fatal("configured password rejected")
	}
	if a.Verify("wrong-password") {
		t.Error("wrong password accepted")
	}
	if a.Verify("") {
		t.Error("empty password accepted")
	}
	if a.Fingerprint() == "" {
		t.Error("empty fingerprint for a configured password")
	}
	// Each boot derives a fresh salt for the login hash, but the fingerprint
	// must be stable for a given password so a restart under the same
	// password keeps sessions.
	b := NewAuth("first-password", "")
	if a.Verify("first-password") && !b.Verify("first-password") {
		t.Error("second instance rejects the same password")
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("fingerprint differs between boots under the same password")
	}
	if c := NewAuth("another-password", ""); c.Fingerprint() == a.Fingerprint() {
		t.Error("different passwords share a fingerprint")
	}
}

func TestAuthVerifiesAPIToken(t *testing.T) {
	a := NewAuth("first-password", "shortcut-token")
	if !a.VerifyAPIToken("shortcut-token") {
		t.Fatal("configured API token rejected")
	}
	if a.VerifyAPIToken("Shortcut-Token") {
		t.Error("API token comparison is not exact")
	}
	if a.VerifyAPIToken("") {
		t.Error("empty API token accepted")
	}
	// The token is held as its SHA-256 digest, never as plaintext.
	if a.apiTokenHash != hashToken("shortcut-token") {
		t.Error("API token not stored as its SHA-256 digest")
	}
	// Without a configured token nothing is accepted — not even the value
	// another deployment might run with.
	off := NewAuth("first-password", "")
	if off.VerifyAPIToken("shortcut-token") || off.VerifyAPIToken("") {
		t.Error("API token verified while bearer auth is disabled")
	}
}

func TestSessionLoadDropsSessionsOnPasswordChange(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	path := filepath.Join(dir, "sessions.json")

	first := NewSessionStore(path, NewAuth("first-password", "").Fingerprint(), clk.Now)
	token, _, _, err := first.Create()
	if err != nil {
		t.Fatal(err)
	}

	// A restart under the same password keeps the session.
	same := NewSessionStore(path, NewAuth("first-password", "").Fingerprint(), clk.Now)
	if err := same.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := same.Lookup(token); !ok {
		t.Error("session dropped despite an unchanged password")
	}

	// A restart under a changed password (a different fingerprint) drops it.
	changed := NewSessionStore(path, NewAuth("second-password", "").Fingerprint(), clk.Now)
	if err := changed.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := changed.Lookup(token); ok {
		t.Error("session survived a password change")
	}
}

func TestSessionLoadRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	for name, content := range map[string]string{
		"wrong shape":  `[{"token_hash":"x","csrf":"y"}]`,
		"corrupt":      "{not json",
		"wrong types":   `{"password":1,"sessions":{}}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		store := NewSessionStore(path, "any-fingerprint", clk.Now)
		if err := store.Load(); err == nil {
			t.Errorf("%s: unreadable session file loaded without error", name)
		}
	}
}

func TestSessionStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	store := NewSessionStore(filepath.Join(dir, "sessions.json"), "test-fingerprint", clk.Now)

	token, csrf, expires, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || csrf == "" || !expires.Equal(clk.Now().Add(sessionTTL)) {
		t.Fatalf("bad session material: %q %q %v", token, csrf, expires)
	}

	sess, ok := store.Lookup(token)
	if !ok || sess.CSRF != csrf {
		t.Fatal("valid session not found")
	}
	if _, ok := store.Lookup("forged-token"); ok {
		t.Error("forged token accepted")
	}
	if store.VerifyCSRF(sess, "wrong") {
		t.Error("wrong CSRF token accepted")
	}
	if !store.VerifyCSRF(sess, csrf) {
		t.Error("correct CSRF token rejected")
	}

	// Logout revokes just this session.
	store.Revoke(token)
	if _, ok := store.Lookup(token); ok {
		t.Error("revoked session still valid")
	}
}

func TestSessionExpiry(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	store := NewSessionStore(filepath.Join(dir, "sessions.json"), "test-fingerprint", clk.Now)
	token, _, _, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(sessionTTL - time.Minute)
	if _, ok := store.Lookup(token); !ok {
		t.Error("session expired too early")
	}
	clk.Advance(2 * time.Minute)
	if _, ok := store.Lookup(token); ok {
		t.Error("expired session still valid")
	}
	// Reload must also drop the expired session from disk.
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Lookup(token); ok {
		t.Error("expired session survived reload")
	}
}

func TestSessionCapEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Unix(1700000000, 0))
	store := NewSessionStore(filepath.Join(dir, "sessions.json"), "test-fingerprint", clk.Now)
	var first string
	for i := 0; i < maxSessions+2; i++ {
		token, _, _, err := store.Create()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = token
		}
		clk.Advance(time.Minute)
	}
	if _, ok := store.Lookup(first); ok {
		t.Error("oldest session not evicted at cap")
	}
}

func TestSessionStoreReloadRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(path, "test-fingerprint", time.Now)
	if err := store.Load(); err == nil {
		t.Error("corrupt sessions file loaded without error")
	}
}

func TestSessionStorePersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	store := NewSessionStore(path, "test-fingerprint", time.Now)
	token, _, _, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewSessionStore(path, "test-fingerprint", time.Now)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup(token); !ok {
		t.Error("session did not survive reload")
	}
	reloaded.RevokeAll()
	if _, ok := reloaded.Lookup(token); ok {
		t.Error("RevokeAll left a session valid")
	}
}

func TestRateLimiterAttempt(t *testing.T) {
	clk := newClock(time.Unix(1700000000, 0))
	limiter := NewRateLimiter(15*time.Minute, 5, 30, clk.Now)
	// Five failures go through, each reported as allowed but unverified.
	for i := 0; i < 5; i++ {
		allowed, verified, _ := limiter.Attempt("1.2.3.4", func() bool { return false }, false)
		if !allowed || verified {
			t.Fatalf("failure %d: allowed=%v verified=%v", i+1, allowed, verified)
		}
	}
	// The sixth is denied — and its credential is never examined.
	invoked := false
	allowed, _, wait := limiter.Attempt("1.2.3.4", func() bool { invoked = true; return true }, false)
	if allowed {
		t.Fatal("blocked after five failures")
	}
	if invoked {
		t.Error("blocked attempt examined the credential")
	}
	if wait <= 0 {
		t.Fatal("wait must be positive while the window is active")
	}
	// A denial records nothing: the five failures stand alone.
	if n := len(limiter.attempts["1.2.3.4"]); n != 5 {
		t.Errorf("denied attempt left %d entries; want 5", n)
	}
	// Other clients are unaffected, and their successful attempts record
	// nothing (a polling client never burns the budget).
	if allowed, verified, _ := limiter.Attempt("5.6.7.8", func() bool { return true }, false); !allowed || !verified {
		t.Fatal("unrelated client blocked")
	}
	if n := len(limiter.attempts["5.6.7.8"]); n != 0 {
		t.Errorf("successful attempt recorded %d entries", n)
	}
	// After the window passes, attempts are allowed again — the entry
	// check prunes the expired failures on its own.
	clk.Advance(16 * time.Minute)
	allowed, _, _ = limiter.Attempt("1.2.3.4", func() bool { return false }, false)
	if !allowed {
		t.Fatal("still blocked after the window expired")
	}
}

func TestRateLimiterAttemptCountsSuccesses(t *testing.T) {
	clk := newClock(time.Unix(1700000000, 0))
	limiter := NewRateLimiter(15*time.Minute, 5, 30, clk.Now)
	// Login-style attempts count successes toward the total cap: thirty
	// minted sessions fit in a window; the thirty-first is denied.
	succeed := func() bool { return true }
	for i := 0; i < 30; i++ {
		allowed, verified, _ := limiter.Attempt("9.9.9.9", succeed, true)
		if !allowed || !verified {
			t.Fatalf("attempt %d denied too early", i+1)
		}
		clk.Advance(time.Second)
	}
	allowed, _, wait := limiter.Attempt("9.9.9.9", succeed, true)
	if allowed {
		t.Fatal("attempt cap not enforced")
	}
	if wait <= 0 {
		t.Fatal("wait must be positive while the window is active")
	}
}

// Nine concurrent failures against a five-failure limit: the check, the
// verification, and the recording are one lock-held operation, so exactly
// five draw through and the rest are denied, however the attempts
// interleave.
func TestRateLimiterAttemptAtomic(t *testing.T) {
	limiter := NewRateLimiter(15*time.Minute, 5, 30, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount, deniedCount := 0, 0
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _, _ := limiter.Attempt("1.2.3.4", func() bool { return false }, false)
			mu.Lock()
			defer mu.Unlock()
			if allowed {
				allowedCount++
			} else {
				deniedCount++
			}
		}()
	}
	wg.Wait()
	if allowedCount != 5 || deniedCount != 4 {
		t.Errorf("got %d allowed and %d denied; want 5 and 4", allowedCount, deniedCount)
	}
}

func TestSplitPHCEdgeCases(t *testing.T) {
	if got := splitPHC("$argon2id$v=19$m=65536,t=2,p=4$salt$hash"); len(got) != 2 || got[0] != "salt" || got[1] != "hash" {
		t.Errorf("splitPHC = %v", got)
	}
	if got := splitPHC("$argon2id$v=19$m=65536,t=2,p=4$onlysalt"); len(got) != 1 {
		t.Errorf("single-segment splitPHC = %v", got)
	}
	if got := splitPHC("garbage"); got != nil {
		t.Errorf("garbage splitPHC = %v", got)
	}
}

func TestWriteFileSyncAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileSync(dir, "data.json", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil || string(data) != `{"x":1}` {
		t.Fatalf("read back %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %v; want 0600", info.Mode().Perm())
	}
	// No temporary leftovers.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "data.json" {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestWriteFileSyncFailsOnUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	// A file squatting on the directory path fails the write for every
	// user — unlike mode bits, which root ignores.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileSync(blocked, "data.json", []byte("x")); err == nil {
		t.Error("write to read-only directory succeeded")
	}
}
