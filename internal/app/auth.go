package app

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	sessionTTL   = 30 * 24 * time.Hour
	maxSessions  = 20
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonSaltLen = 16
	argonKeyLen  = 32
)

// randomToken returns n random bytes, URL-safe base64 encoded.
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashPassword derives an Argon2id hash and encodes it in PHC string format.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword checks a password against a PHC-format Argon2id hash in
// constant time. Malformed hashes simply fail.
func verifyPassword(encoded, password string) bool {
	var version int
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(encoded, "$argon2id$v=%d$m=%d,t=%d,p=%d$", &version, &memory, &timeCost, &threads); err != nil {
		return false
	}
	parts := splitPHC(encoded)
	if len(parts) != 2 || version != argon2.Version || memory == 0 || timeCost == 0 || threads == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// splitPHC returns the salt and hash segments of a PHC string.
func splitPHC(encoded string) []string {
	start, fields := 0, 0
	var segments []string
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '$' {
			fields++
			if fields == 4 {
				start = i + 1
				segments = []string{encoded[start:], ""}
				for j := start; j < len(encoded); j++ {
					if encoded[j] == '$' {
						segments[0], segments[1] = encoded[start:j], encoded[j+1:]
						return segments
					}
				}
				return segments[:1]
			}
		}
	}
	return nil
}

// fingerprintSalt is a fixed salt for the password fingerprint. Unlike the
// login hash it must be deterministic across boots — sessions survive
// restarts under an unchanged password — but it stays argon2id-expensive so
// the value stored in sessions.json cannot be brute-forced offline any
// faster than the login hash itself.
var fingerprintSalt = []byte("personal-yt-downloader/fp/v1")

// fingerprintPassword derives the stable per-password fingerprint.
func fingerprintPassword(password string) string {
	key := argon2.IDKey([]byte(password), fingerprintSalt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return hashToken(string(key))
}

// Auth verifies the single login password. The password is configured
// exclusively through the PASSWORD environment variable: it is hashed
// once at boot and never written to disk.
type Auth struct {
	hash string
	fp   string // stable fingerprint of the password for session bookkeeping
}

// NewAuth derives the stored hash for the boot-time password. An empty
// password is a deployment error; serve refuses to start, so a running
// server always holds a usable Auth.
func NewAuth(password string) *Auth {
	encoded, err := hashPassword(password)
	if err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return &Auth{hash: encoded, fp: fingerprintPassword(password)}
}

// Verify reports whether the password matches the configured one.
func (a *Auth) Verify(password string) bool {
	return a.hash != "" && verifyPassword(a.hash, password)
}

// Fingerprint identifies the configured password without revealing it.
// Sessions issued under a different fingerprint are dropped when the store
// loads, so changing PASSWORD revokes every existing session.
func (a *Auth) Fingerprint() string { return a.fp }

// Session is one authenticated browser session, persisted server-side. Only
// the SHA-256 hash of the session token is ever stored or logged.
type Session struct {
	TokenHash string    `json:"token_hash"`
	CSRF      string    `json:"csrf"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionFile is the on-disk shape; the password fingerprint rides along so
// a restart under a changed PASSWORD drops every stored session instead
// of honoring cookies minted under the old password.
type sessionFile struct {
	Password string    `json:"password"`
	Sessions []Session `json:"sessions"`
}

// SessionStore issues and verifies sessions, persisted atomically.
type SessionStore struct {
	mu       sync.Mutex
	path     string
	fp       string // fingerprint of the password that may mint sessions
	sessions map[string]Session // keyed by token hash
	now      func() time.Time
}

func NewSessionStore(path, fingerprint string, now func() time.Time) *SessionStore {
	if now == nil {
		now = time.Now
	}
	return &SessionStore{path: path, fp: fingerprint, sessions: map[string]Session{}, now: now}
}

func (s *SessionStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file sessionFile
	if err := jsonUnmarshalStrict(data, &file); err != nil {
		// An unreadable session file denies all sessions; re-login fixes it.
		return errors.New("sessions.json is unreadable")
	}
	fresh := map[string]Session{}
	if file.Password == s.fp {
		for _, sess := range file.Sessions {
			if sess.ExpiresAt.After(s.now()) && sess.TokenHash != "" && sess.CSRF != "" {
				fresh[sess.TokenHash] = sess
			}
		}
	} else if len(file.Sessions) > 0 {
		slog.Info("Stored sessions dropped after a password change")
	}
	s.sessions = fresh
	return nil
}

func (s *SessionStore) persistLocked() error {
	file := sessionFile{Password: s.fp, Sessions: make([]Session, 0, len(s.sessions))}
	for _, sess := range s.sessions {
		file.Sessions = append(file.Sessions, sess)
	}
	data, err := jsonMarshalStrict(file)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileSync(dir, filepath.Base(s.path), data)
}

// Create issues a new session for a valid login and returns the opaque
// session token and the CSRF token the client must echo on mutations.
func (s *SessionStore) Create() (token, csrf string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if len(s.sessions) >= maxSessions {
		var oldestKey string
		var oldest time.Time
		for key, sess := range s.sessions {
			if oldestKey == "" || sess.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, sess.CreatedAt
			}
		}
		delete(s.sessions, oldestKey)
	}
	token, csrf = randomToken(32), randomToken(32)
	sess := Session{TokenHash: hashToken(token), CSRF: csrf, CreatedAt: now, ExpiresAt: now.Add(sessionTTL)}
	s.sessions[sess.TokenHash] = sess
	if err := s.persistLocked(); err != nil {
		delete(s.sessions, sess.TokenHash)
		return "", "", time.Time{}, err
	}
	return token, csrf, sess.ExpiresAt, nil
}

// Lookup validates a session token and reports the session when valid.
func (s *SessionStore) Lookup(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[hashToken(token)]
	if !ok || !sess.ExpiresAt.After(s.now()) {
		if ok {
			delete(s.sessions, sess.TokenHash)
			_ = s.persistLocked()
		}
		return Session{}, false
	}
	return sess, true
}

// VerifyCSRF checks the request's CSRF token against the session's.
func (s *SessionStore) VerifyCSRF(sess Session, token string) bool {
	return subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(token)) == 1
}

// Revoke deletes one session (logout).
func (s *SessionStore) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[hashToken(token)]; !ok {
		return
	}
	delete(s.sessions, hashToken(token))
	_ = s.persistLocked()
}

// RevokeAll drops every session (password change).
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]Session{}
	_ = s.persistLocked()
}

func (s *SessionStore) pruneLocked(now time.Time) {
	expired := false
	for key, sess := range s.sessions {
		if !sess.ExpiresAt.After(now) {
			delete(s.sessions, key)
			expired = true
		}
	}
	if expired {
		_ = s.persistLocked()
	}
}

// Prune drops expired sessions; called from a background loop.
func (s *SessionStore) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
}

// RateLimiter bounds login attempts per client. Failures block quickly;
// total attempts are also capped so token minting cannot be spammed.
type RateLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	window      time.Duration
	maxFailures int
	maxAttempts int
	attempts    map[string][]rateAttempt
}

type rateAttempt struct {
	at  time.Time
	bad bool
}

func NewRateLimiter(window time.Duration, maxFailures, maxAttempts int, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{now: now, window: window, maxFailures: maxFailures, maxAttempts: maxAttempts, attempts: map[string][]rateAttempt{}}
}

// Allowed reports whether a login attempt may proceed. When denied it returns
// how long to wait.
func (r *RateLimiter) Allowed(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.recordLocked(key, now, false)
	return r.checkLocked(key, now)
}

// RecordBad marks a failed login attempt.
func (r *RateLimiter) RecordBad(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.recordLocked(key, now, true)
}

func (r *RateLimiter) recordLocked(key string, now time.Time, bad bool) {
	list := r.attempts[key]
	list = append(list, rateAttempt{at: now, bad: bad})
	// Keep only the current window's attempts.
	kept := list[:0]
	for _, a := range list {
		if now.Sub(a.at) < r.window {
			kept = append(kept, a)
		}
	}
	r.attempts[key] = kept
}

func (r *RateLimiter) checkLocked(key string, now time.Time) (bool, time.Duration) {
	var failures, attempts int
	var oldestBad, oldestAny time.Time
	for _, a := range r.attempts[key] {
		attempts++
		if oldestAny.IsZero() || a.at.Before(oldestAny) {
			oldestAny = a.at
		}
		if a.bad {
			failures++
			if oldestBad.IsZero() || a.at.Before(oldestBad) {
				oldestBad = a.at
			}
		}
	}
	if failures >= r.maxFailures {
		if wait := r.window - now.Sub(oldestBad); wait > 0 {
			return false, wait
		}
		return false, time.Second
	}
	if attempts >= r.maxAttempts {
		if wait := r.window - now.Sub(oldestAny); wait > 0 {
			return false, wait
		}
		return false, time.Second
	}
	return true, 0
}
