package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "session"
	csrfHeader    = "X-CSRF-Token"
	maxBodyBytes  = 8192
)

// Server wires the manager, stores, and embedded frontend into an
// http.Handler.
type Server struct {
	manager        *Manager
	store          Store
	auth           *Auth
	sessions       *SessionStore
	links          *LinkStore
	limiter        *RateLimiter
	insecureCookie bool
	trustedProxies []netip.Prefix // peers allowed to set X-Forwarded-For
	assets         fs.FS          // embedded frontend build (index.html, assets/)
}

func NewServer(manager *Manager, store Store, auth *Auth, sessions *SessionStore, links *LinkStore, limiter *RateLimiter, insecureCookie bool, trustedProxies []netip.Prefix, assets fs.FS) *Server {
	return &Server{
		manager: manager, store: store, auth: auth, sessions: sessions, links: links,
		limiter: limiter, insecureCookie: insecureCookie, trustedProxies: trustedProxies, assets: assets,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/session", s.requireSession(s.handleSession))
	mux.HandleFunc("POST /api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("GET /api/jobs", s.requireAuth(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.requireAuth(s.handleCreateJob))
	mux.HandleFunc("GET /api/jobs/{id}", s.requireAuth(s.handleGetJob))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.requireAuth(s.handleRetryJob))
	mux.HandleFunc("DELETE /api/jobs/{id}", s.requireAuth(s.handleDeleteJob))
	mux.HandleFunc("GET /api/jobs/{id}/download", s.requireAuth(s.handleDownload))
	mux.HandleFunc("POST /api/jobs/{id}/link", s.requireAuth(s.handleShareLink))
	// The optional segment after the token is the real filename: clients
	// that save from a URL (VLC, the iOS Files app) name the file after the
	// last path segment, so the issued URL ends in the video's title. The
	// token alone is the credential; the name is decorative and never
	// consulted, which is why both shapes reach the same handler.
	mux.HandleFunc("GET /m/{token}", s.handlePublicMedia)
	mux.HandleFunc("GET /m/{token}/{name...}", s.handlePublicMedia)

	s.serveFrontend(mux)

	return requestLogging(s.securityHeaders(mux))
}

// ---- authentication ----

// ParseTrustedProxies parses the -trusted-proxies flag: a comma-separated
// list of proxy IPs or CIDR ranges. A bare IP becomes a /32 (or /128)
// prefix. An empty spec trusts no proxy at all.
func ParseTrustedProxies(spec string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !strings.Contains(field, "/") {
			addr, err := netip.ParseAddr(field)
			if err != nil {
				return nil, fmt.Errorf("invalid -trusted-proxies entry %q", field)
			}
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, fmt.Errorf("invalid -trusted-proxies entry %q", field)
		}
		if prefix.Addr().Is4In6() {
			// Peers and X-Forwarded-For entries are normalized to plain IPv4
			// before matching, so a v4-mapped prefix must translate to its
			// plain form too or it never matches anything. The mapped marker
			// ::ffff:0:0 itself spans the first 96 bits; a shorter prefix
			// reaches outside the mapped range and has no IPv4 form.
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("invalid -trusted-proxies entry %q: an IPv4-mapped range needs at least /96; write it in plain IPv4", field)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// clientIP derives the rate-limiting key. X-Forwarded-For is honored only
// when the direct peer is a configured trusted proxy — and then only the
// suffix a proxy appended: entries are walked right to left, skipping
// trusted proxies, because the leftmost entry is client-controlled.
func (s *Server) clientIP(r *http.Request) string {
	peer := remoteAddrHost(r.RemoteAddr)
	peerIP, err := netip.ParseAddr(peer)
	if err != nil {
		return peer // unparseable peer: key on the raw string, XFF ignored
	}
	peerIP = peerIP.Unmap()
	if !s.trustedProxy(peerIP) {
		return peer // direct client (LAN, Tailscale, internet): its own address
	}
	// Repeated X-Forwarded-For lines are one chain: appending proxies add
	// each hop as a new header line, so the earliest (client-controlled)
	// entries sit in the first lines and the appended tail in the last —
	// exactly the order the right-to-left walk relies on. Header.Get would
	// see only the first line.
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return peer
	}
	entries := strings.Split(strings.Join(values, ","), ",")
	for i := len(entries) - 1; i >= 0; i-- {
		entry, err := netip.ParseAddr(strings.TrimSpace(entries[i]))
		if err != nil {
			// A real proxy chain cannot produce this; trust nothing past it.
			break
		}
		entry = entry.Unmap()
		if s.trustedProxy(entry) {
			continue
		}
		return entry.String()
	}
	return peer // no XFF, or every entry was a trusted proxy
}

// remoteAddrHost extracts the host part of RemoteAddr, falling back to the
// raw string when it does not parse.
func remoteAddrHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func (s *Server) trustedProxy(ip netip.Addr) bool {
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// checkOrigin rejects cross-site state changes when an Origin header is
// present (belt and braces alongside SameSite=Strict and the CSRF token).
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !checkOrigin(r) {
		apiError(w, http.StatusForbidden, "Cross-site requests are not allowed.")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if err := s.parseBody(w, r, &input); err != nil {
		return
	}
	ip := s.clientIP(r)
	// One atomic limiter operation: the block check, the password
	// comparison, and the failure recording cannot be interleaved by
	// concurrent requests. Verification runs under the limiter's lock —
	// the Argon2id comparison is slow, but it also bounds concurrent
	// verifications to one. Successful logins still count toward the
	// attempt cap: minting sessions must cost budget.
	allowed, verified, wait := s.limiter.Attempt(ip, func() bool {
		return s.auth.Verify(input.Password)
	}, true)
	if !allowed {
		tooManyAttempts(w, wait)
		return
	}
	if !verified {
		apiError(w, http.StatusUnauthorized, "Incorrect password.")
		return
	}
	token, csrf, expires, err := s.sessions.Create()
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   !s.insecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, struct {
		OK        bool      `json:"ok"`
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}{true, csrf, expires})
	slog.Info("Login succeeded")
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess := r.Context().Value(sessionKey{}).(Session)
	writeJSON(w, http.StatusOK, struct {
		OK        bool      `json:"ok"`
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}{true, sess.CSRF, sess.ExpiresAt})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !s.insecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	slog.Info("Logout")
}

type sessionKey struct{}

// requireSession authenticates the session cookie and enforces the CSRF
// token on state-changing methods.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rejectOpaquePath(w, r) {
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			apiError(w, http.StatusUnauthorized, ErrAuth.Error())
			return
		}
		sess, ok := s.sessions.Lookup(cookie.Value)
		if !ok {
			apiError(w, http.StatusUnauthorized, ErrAuth.Error())
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			if !checkOrigin(r) || !s.sessions.VerifyCSRF(sess, r.Header.Get(csrfHeader)) {
				apiError(w, http.StatusForbidden, "A valid CSRF token is required.")
				return
			}
		}
		next(w, r.WithContext(contextWithSession(r, sess)))
	}
}

func contextWithSession(r *http.Request, sess Session) context.Context {
	return context.WithValue(r.Context(), sessionKey{}, sess)
}

// rejectOpaquePath turns away paths containing "//": an empty segment makes
// the mux's pattern matching behave differently from a cleaned path, so
// authenticated routes decline the ambiguity up front.
func rejectOpaquePath(w http.ResponseWriter, r *http.Request) bool {
	if strings.Contains(r.URL.Path, "//") {
		apiError(w, http.StatusNotFound, "Route not found.")
		return true
	}
	return false
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. It reports a Bearer challenge whenever the scheme names Bearer —
// including the malformed no-token form, which must draw a 401 rather than
// silently falling back to session auth. Absent headers and other schemes
// report false; those requests fall through to session auth.
func bearerToken(r *http.Request) (string, bool) {
	scheme, rest, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// tooManyAttempts writes the 429 shared by every throttled auth attempt.
func tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	apiError(w, http.StatusTooManyRequests, "Too many attempts. Try again later.")
}

// requireAuth guards the API routes, accepting either a Bearer token (the
// API_TOKEN credential for direct API requests) or a session cookie. A
// request carrying an Authorization: Bearer header is authorized by that
// token alone: no CSRF token or Origin check applies, since nothing
// cookie-shaped is involved and a cross-site page cannot attach the
// header — and a failed or malformed token is a 401 even alongside a valid
// session, so the header never silently degrades into cookie auth. Token
// attempts share the login limiter's per-IP budget with one difference
// from logins: successful attempts record nothing at all, so a polling
// client never burns the budget. The limiter adjudicates each attempt
// atomically — block check, token comparison, failure recording under one
// lock — so concurrent guesses cannot slip past the failure limit, and a
// blocked IP is turned away before its token is examined, drawing the
// same 429 for a wrong and a right guess.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	sessionFlow := s.requireSession(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok {
			if rejectOpaquePath(w, r) {
				return
			}
			allowed, verified, wait := s.limiter.Attempt(s.clientIP(r), func() bool {
				return s.auth.VerifyAPIToken(token)
			}, false)
			if !allowed {
				tooManyAttempts(w, wait)
				return
			}
			if !verified {
				apiError(w, http.StatusUnauthorized, "Invalid API token.")
				return
			}
			next(w, r)
			return
		}
		sessionFlow(w, r)
	}
}

// ---- jobs ----

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, total, err := s.manager.List()
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Jobs       []jobView `json:"jobs"`
		TotalBytes int64     `json:"total_bytes"`
	}{jobs, total})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL string `json:"url"`
	}
	if err := s.parseBody(w, r, &input); err != nil {
		return
	}
	id, err := VideoID(input.URL)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	j, duplicate, err := s.manager.Submit(id)
	if err != nil {
		s.submitError(w, err)
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, struct {
			Job       jobView `json:"job"`
			Duplicate bool    `json:"duplicate"`
		}{j, true})
		return
	}
	w.Header().Set("Location", "/api/jobs/"+id)
	writeJSON(w, http.StatusAccepted, struct {
		Job       jobView `json:"job"`
		Duplicate bool    `json:"duplicate"`
	}{j, false})
}

func (s *Server) submitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBusy):
		w.Header().Set("Retry-After", "10")
		apiError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ErrClosed):
		apiError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrConflict):
		apiError(w, http.StatusConflict, err.Error())
	default:
		apiError(w, http.StatusInsufficientStorage, ErrStorage.Error())
	}
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		apiError(w, http.StatusBadRequest, "Invalid YouTube video ID.")
		return
	}
	j, err := s.manager.Get(id)
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Job jobView `json:"job"`
	}{j})
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		apiError(w, http.StatusBadRequest, "Invalid YouTube video ID.")
		return
	}
	j, err := s.manager.Retry(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			apiError(w, http.StatusNotFound, ErrNotFound.Error())
			return
		}
		s.submitError(w, err)
		return
	}
	s.links.Revoke(id) // a fresh download invalidates previously issued links
	writeJSON(w, http.StatusOK, struct {
		Job jobView `json:"job"`
	}{j})
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		apiError(w, http.StatusBadRequest, "Invalid YouTube video ID.")
		return
	}
	if err := s.manager.Delete(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			apiError(w, http.StatusNotFound, ErrNotFound.Error())
			return
		}
		if errors.Is(err, ErrStorage) {
			apiError(w, http.StatusInternalServerError, ErrStorage.Error())
			return
		}
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.links.Revoke(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	slog.Info("Job deleted via API", "id", id)
}

// handleShareLink issues a fresh 24-hour secret link for one completed
// video. The token itself is only ever returned to the requesting client.
func (s *Server) handleShareLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		apiError(w, http.StatusBadRequest, "Invalid YouTube video ID.")
		return
	}
	j, err := s.manager.Get(id)
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	if j.Status != StatusReady {
		apiError(w, http.StatusConflict, "The file is not ready.")
		return
	}
	token, expires, err := s.links.Issue(id, j.CreatedAt)
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	// Appending the real filename keeps the download name when the link is
	// saved or opened from a URL: players and the Files app name the file
	// after the last path segment, not the Content-Disposition header.
	name := url.PathEscape(SafeFilename(j.Title, j.ID))
	writeJSON(w, http.StatusOK, struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}{"/m/" + token + "/" + name, expires})
}

// ---- file delivery ----

// serveMedia streams a completed MP4 straight from the filesystem with
// attachment disposition, HEAD, and Range support.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, j jobView, open func() (File, time.Time, error)) {
	file, modified, err := open()
	if err != nil {
		apiError(w, http.StatusNotFound, "The file is unavailable.")
		return
	}
	defer file.Close()
	name := SafeFilename(j.Title, j.ID)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, modified, file)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		apiError(w, http.StatusBadRequest, "Invalid YouTube video ID.")
		return
	}
	j, err := s.manager.Get(id)
	if errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, ErrStorage.Error())
		return
	}
	if j.Status != StatusReady {
		apiError(w, http.StatusConflict, "The file is not ready.")
		return
	}
	s.serveMedia(w, r, j, func() (File, time.Time, error) { return s.store.OpenVideo(id) })
}

// handlePublicMedia serves a share link. It requires no session: the random
// token in the path is the credential. Invalid, expired, and revoked links
// are all indistinguishable 404s, and the token never appears in logs.
func (s *Server) handlePublicMedia(w http.ResponseWriter, r *http.Request) {
	videoID, incarnation, ok := s.links.Lookup(r.PathValue("token"))
	if !ok {
		apiError(w, http.StatusNotFound, "Not found.")
		return
	}
	j, err := s.manager.Get(videoID)
	if err != nil || j.Status != StatusReady || !j.CreatedAt.Equal(incarnation) {
		apiError(w, http.StatusNotFound, "Not found.")
		return
	}
	s.serveMedia(w, r, j, func() (File, time.Time, error) { return s.store.OpenVideo(videoID) })
}

// ---- helpers ----

// parseBody enforces a single JSON object with known fields.
func (s *Server) parseBody(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json.")
		return err
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.bodyError(w, err)
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		s.bodyError(w, err)
		return err
	}
	return nil
}

func (s *Server) bodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		apiError(w, http.StatusRequestEntityTooLarge, "The request body is too large.")
		return
	}
	apiError(w, http.StatusBadRequest, "Provide exactly one JSON object with the expected fields.")
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			// API responses, media, and the shell must bypass caches.
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

// ---- frontend ----

// serveFrontend serves the embedded single-page app: the shell at / with
// no-store so new deploys are picked up, and content-hashed assets with an
// immutable cache policy.
func (s *Server) serveFrontend(mux *http.ServeMux) {
	assets := s.assets
	if assets == nil {
		return
	}
	sub, err := fs.Sub(assets, "assets")
	if err == nil {
		fileServer := http.StripPrefix("/assets/", http.FileServerFS(sub))
		mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/assets/" || strings.Contains(r.URL.Path, "//") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			fileServer.ServeHTTP(w, r)
		})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(assets, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}
