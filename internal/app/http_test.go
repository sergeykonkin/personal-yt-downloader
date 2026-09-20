package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const testPassword = "swordfish"

// defaultTrustedProxies parses the -trusted-proxies default: the test
// servers are reached over loopback, so this matches what production runs
// with unless the flag is changed.
func defaultTrustedProxies(t *testing.T) []netip.Prefix {
	t.Helper()
	prefixes, err := ParseTrustedProxies("127.0.0.0/8,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	return prefixes
}

// httpEnv is a fully wired server over fakes plus an authenticated client.
type httpEnv struct {
	t        *testing.T
	ts       *httptest.Server
	client   *http.Client
	manager  *Manager
	store    *memoryStore
	auth     *Auth
	dir      string
	sessions *SessionStore
	links    *LinkStore
	limiter  *RateLimiter
	clk      *clock
	assets   fstest.MapFS
	csrf     string
}

func newHTTPEnv(t *testing.T, dl Downloader, fetch MetadataFetcher) *httpEnv {
	t.Helper()
	dir := t.TempDir()
	store := newMemoryStore()
	clk := newClock(time.Unix(1700000000, 0))
	if dl == nil {
		dl = downloadFunc(func(context.Context, Job, Reporter) (DownloadResult, error) {
			return DownloadResult{Title: "Big Buck Bunny", SizeBytes: 12345}, nil
		})
	}
	if fetch == nil {
		fetch = fetchFunc(func(_ context.Context, id string) (Metadata, error) {
			return Metadata{ID: id, Title: "Big Buck Bunny", DurationSec: 634, LiveStatus: "not_live"}, nil
		})
	}
	manager := newTestManager(store, dl, fetch)
	auth := NewAuth(testPassword, "")
	sessions := NewSessionStore(filepath.Join(dir, "sessions.json"), auth.Fingerprint(), clk.Now)
	links := NewLinkStore(filepath.Join(dir, "links.json"), clk.Now)
	limiter := NewRateLimiter(time.Minute, 5, 30, clk.Now)
	assets := fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<!doctype html><title>app</title>")},
		"assets/app.js":  &fstest.MapFile{Data: []byte("console.log('app')")},
		"assets/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	server := NewServer(manager, store, auth, sessions, links, limiter, true, defaultTrustedProxies(t), assets)
	ts := httptest.NewServer(server.Handler())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &httpEnv{
		t: t, ts: ts, client: &http.Client{Jar: jar},
		manager: manager, store: store, auth: auth, dir: dir,
		sessions: sessions, links: links, limiter: limiter, clk: clk, assets: assets,
	}
	t.Cleanup(func() { ts.Close() })
	t.Cleanup(func() { manager.Close() })
	return e
}

// issue performs a request with an optional JSON body and extra headers
// against any URL, using the given client.
func issue(t *testing.T, client *http.Client, method, url string, body any, header map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// do issues a session-authenticated request with JSON body handling and the
// CSRF header applied automatically.
func (e *httpEnv) do(method, path string, body any) *http.Response {
	e.t.Helper()
	header := map[string]string{}
	if e.csrf != "" && method != http.MethodGet && method != http.MethodHead {
		header[csrfHeader] = e.csrf
	}
	return issue(e.t, e.client, method, e.ts.URL+path, body, header)
}

// bare issues a request with no cookies — the share-link client.
func (e *httpEnv) bare(method, path string, header map[string]string) *http.Response {
	e.t.Helper()
	return issue(e.t, http.DefaultClient, method, e.ts.URL+path, nil, header)
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
}

func decodeJSON(t *testing.T, res *http.Response, v any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decoding %s %s: %v", res.Request.Method, res.Request.URL.Path, err)
	}
}

func (e *httpEnv) login() {
	e.t.Helper()
	res := e.do("POST", "/api/login", map[string]string{"password": testPassword})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		e.t.Fatalf("login = %d: %s", res.StatusCode, body)
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	decodeJSON(e.t, res, &out)
	if out.CSRFToken == "" {
		e.t.Fatal("login returned no CSRF token")
	}
	e.csrf = out.CSRFToken
}

func (e *httpEnv) submit(t *testing.T, url string) (string, int) {
	t.Helper()
	res := e.do("POST", "/api/jobs", map[string]string{"url": url})
	defer res.Body.Close()
	var out struct {
		Job       jobView `json:"job"`
		Duplicate bool    `json:"duplicate"`
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("submit = %d: %s", res.StatusCode, body)
	}
	decodeJSON(t, res, &out)
	return out.Job.ID, res.StatusCode
}

func (e *httpEnv) waitStatus(t *testing.T, id, want string) jobView {
	t.Helper()
	var last jobView
	waitFor(t, "job "+id+" to reach "+want, func() bool {
		res := e.do("GET", "/api/jobs/"+id, nil)
		if res.StatusCode != http.StatusOK {
			drain(res)
			return false
		}
		var out struct {
			Job jobView `json:"job"`
		}
		decodeJSON(t, res, &out)
		last = out.Job
		return last.Status == want
	})
	return last
}

func makeVideo(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

const goodURL = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

// ---- basics ----

func TestHTTPHealthAndFrontendHeaders(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)

	res := e.do("GET", "/healthz", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
	var health map[string]string
	decodeJSON(t, res, &health)
	if health["status"] != "ok" {
		t.Errorf("healthz body = %v", health)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("healthz %s = %q; want %q", header, got, want)
		}
	}

	res = e.do("GET", "/", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("index = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if body, _ := io.ReadAll(res.Body); !bytes.Contains(body, []byte("<title>app</title>")) {
		t.Error("index body wrong")
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("index cache = %q", res.Header.Get("Cache-Control"))
	}

	res = e.do("GET", "/assets/app.js", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("asset = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset cache = %q", cc)
	}

	// Unknown share tokens are plain 404s without a session.
	res = e.bare("GET", "/m/doesnotexist", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("bad share token = %d", res.StatusCode)
	}
}

// ---- auth ----

func TestHTTPLoginUnauthenticated(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	res := e.do("GET", "/api/jobs", nil)
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated list = %d", res.StatusCode)
	}
}

func TestHTTPLoginWrongPasswordThenRight(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	res := e.do("POST", "/api/login", map[string]string{"password": "wrong"})
	var out map[string]string
	decodeJSON(t, res, &out)
	if res.StatusCode != http.StatusUnauthorized || out["error"] != "Incorrect password." {
		t.Errorf("wrong password = %d %v", res.StatusCode, out)
	}
	e.login()
	res = e.do("GET", "/api/session", nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("session after login = %d", res.StatusCode)
	}
}

func TestHTTPLoginRateLimit(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	for i := 0; i < 5; i++ {
		res := e.do("POST", "/api/login", map[string]string{"password": "wrong"})
		drain(res)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, res.StatusCode)
		}
	}
	res := e.do("POST", "/api/login", map[string]string{"password": testPassword})
	drain(res)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked login = %d; want 429", res.StatusCode)
	}
	if ra := res.Header.Get("Retry-After"); ra == "" {
		t.Error("Retry-After missing on 429")
	}
	// A different client IP is unaffected.
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	other, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(other)
	if other.StatusCode != http.StatusOK {
		t.Errorf("other IP login = %d", other.StatusCode)
	}
	// The window passes and the original IP may try again.
	e.clk.Advance(time.Minute + time.Second)
	res = e.do("POST", "/api/login", map[string]string{"password": testPassword})
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("login after window = %d", res.StatusCode)
	}
}

// The same atomicity on the password path: six simultaneous wrong-password
// logins against the five-failure limit draw exactly five 401s and one 429,
// however the requests interleave.
func TestHTTPLoginConcurrentAttempts(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)

	var wg sync.WaitGroup
	var mu sync.Mutex
	unauthorized, throttled := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("POST", e.ts.URL+"/api/login",
				strings.NewReader(`{"password":"wrong"}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			drain(res)
			mu.Lock()
			defer mu.Unlock()
			switch res.StatusCode {
			case http.StatusUnauthorized:
				unauthorized++
			case http.StatusTooManyRequests:
				throttled++
			default:
				t.Errorf("concurrent login = %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	if unauthorized != 5 || throttled != 1 {
		t.Errorf("concurrent logins: %d unauthorized and %d throttled; want 5 and 1", unauthorized, throttled)
	}
}

// ---- bearer auth (direct API requests) ----

const testAPIToken = "direct-api-token"

// tokenServer returns a second server over the same fakes whose Auth accepts
// the given API token — the deployment shape a direct API client faces.
func (e *httpEnv) tokenServer(token string) *httptest.Server {
	e.t.Helper()
	auth := NewAuth(testPassword, token)
	server := NewServer(e.manager, e.store, auth, e.sessions, e.links, e.limiter, true, defaultTrustedProxies(e.t), e.assets)
	ts := httptest.NewServer(server.Handler())
	e.t.Cleanup(func() { ts.Close() })
	return ts
}

// doBearer issues a cookie-less request carrying an Authorization header —
// what a direct API client (curl, iOS Shortcuts) sends.
func doBearer(t *testing.T, ts *httptest.Server, method, path, authorization string, body any) *http.Response {
	t.Helper()
	return issue(t, http.DefaultClient, method, ts.URL+path, body, map[string]string{"Authorization": authorization})
}

func TestHTTPBearerFullAPIAccess(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)
	auth := "Bearer " + testAPIToken

	// Submitting needs neither a cookie, nor a CSRF token, nor an Origin.
	res := doBearer(t, ts, "POST", "/api/jobs", auth, map[string]string{"url": goodURL})
	var out struct {
		Job jobView `json:"job"`
	}
	if res.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("submit with token = %d: %s", res.StatusCode, body)
	}
	decodeJSON(t, res, &out)
	id := out.Job.ID

	// A foreign Origin changes nothing on the token path: CSRF defenses
	// guard cookie auth, and a cross-site page cannot attach the header.
	req, _ := http.NewRequest("POST", ts.URL+"/api/jobs",
		strings.NewReader(`{"url":"https://www.youtube.com/watch?v=`+videoID(1)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	req.Header.Set("Origin", "https://evil.example")
	evil, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(evil)
	if evil.StatusCode != http.StatusAccepted {
		t.Errorf("bearer submit with foreign Origin = %d", evil.StatusCode)
	}

	// The whole lifecycle is reachable: poll to ready, fetch the file,
	// mint a share link, and delete.
	waitFor(t, "job to become ready", func() bool {
		res := doBearer(t, ts, "GET", "/api/jobs/"+id, auth, nil)
		if res.StatusCode != http.StatusOK {
			drain(res)
			return false
		}
		var got struct {
			Job jobView `json:"job"`
		}
		decodeJSON(t, res, &got)
		return got.Job.Status == StatusReady
	})
	e.store.setFile(id, makeVideo(64))

	res = doBearer(t, ts, "GET", "/api/jobs/"+id+"/download", auth, nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || len(body) != 64 {
		t.Errorf("download with token = %d (%d bytes)", res.StatusCode, len(body))
	}

	res = doBearer(t, ts, "POST", "/api/jobs/"+id+"/link", auth, nil)
	var link struct {
		URL string `json:"url"`
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("share link with token = %d", res.StatusCode)
	}
	decodeJSON(t, res, &link)
	if link.URL == "" {
		t.Error("no share link issued")
	}

	res = doBearer(t, ts, "DELETE", "/api/jobs/"+id, auth, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("delete with token = %d", res.StatusCode)
	}
}

func TestHTTPBearerRejectsBadTokens(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)

	// A wrong token is a 401 with the token-specific message.
	res := doBearer(t, ts, "GET", "/api/jobs", "Bearer wrong-token", nil)
	var out map[string]string
	decodeJSON(t, res, &out)
	if res.StatusCode != http.StatusUnauthorized || out["error"] != "Invalid API token." {
		t.Errorf("wrong token = %d %v", res.StatusCode, out)
	}

	// The session endpoints stay cookie-only: the token grants the API,
	// not session management.
	res = doBearer(t, ts, "GET", "/api/session", "Bearer "+testAPIToken, nil)
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("session endpoint with token = %d", res.StatusCode)
	}

	// Once a Bearer header is present it is the only credential tried:
	// a bad token is a 401 even alongside a valid session. The test
	// servers share the 127.0.0.1 cookie domain, so e.client's session
	// cookie rides along on this request.
	e.login()
	req, _ := http.NewRequest("GET", ts.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token with valid session = %d", res.StatusCode)
	}

	// Conversely, a valid token authenticates without the CSRF handshake —
	// the cookie plays no role on the token path.
	req, _ = http.NewRequest("POST", ts.URL+"/api/jobs",
		strings.NewReader(`{"url":"https://www.youtube.com/watch?v=`+videoID(2)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	res, err = e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != http.StatusAccepted {
		t.Errorf("token submit with session but no CSRF = %d", res.StatusCode)
	}
}

func TestHTTPBearerRateLimited(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)

	// Five bad tokens block the sixth attempt — the same per-IP budget as
	// failed logins.
	for i := 0; i < 5; i++ {
		res := doBearer(t, ts, "GET", "/api/jobs", "Bearer wrong-token", nil)
		drain(res)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, res.StatusCode)
		}
	}
	res := doBearer(t, ts, "GET", "/api/jobs", "Bearer wrong-token", nil)
	drain(res)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked token attempt = %d; want 429", res.StatusCode)
	}
	if ra := res.Header.Get("Retry-After"); ra == "" {
		t.Error("Retry-After missing on 429")
	}

	// A blocked IP is rejected before its token is examined: even the
	// correct token draws the same 429, so brute force gets no oracle for
	// which guess landed.
	res = doBearer(t, ts, "GET", "/api/jobs", "Bearer "+testAPIToken, nil)
	drain(res)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("valid token while blocked = %d; want 429", res.StatusCode)
	}

	// The token failures share the login budget for this IP.
	res = e.do("POST", "/api/login", map[string]string{"password": testPassword})
	drain(res)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("login after token failures = %d; want 429", res.StatusCode)
	}

	// Once the window passes, both credentials work again.
	e.clk.Advance(time.Minute + time.Second)
	res = doBearer(t, ts, "GET", "/api/jobs", "Bearer "+testAPIToken, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("token after window = %d", res.StatusCode)
	}
	res = e.do("POST", "/api/login", map[string]string{"password": testPassword})
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("login after window = %d", res.StatusCode)
	}
}

// Nine simultaneous wrong-token guesses against the five-failure limit: the
// check, verification, and recording are one atomic limiter operation, so
// exactly five draw a 401 and the other four are throttled, however the
// requests interleave — none slip past the limit between the check and the
// record.
func TestHTTPBearerConcurrentAttempts(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)

	var wg sync.WaitGroup
	var mu sync.Mutex
	unauthorized, throttled := 0, 0
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("GET", ts.URL+"/api/jobs", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer wrong-token")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			drain(res)
			mu.Lock()
			defer mu.Unlock()
			switch res.StatusCode {
			case http.StatusUnauthorized:
				unauthorized++
			case http.StatusTooManyRequests:
				throttled++
			default:
				t.Errorf("concurrent token attempt = %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()
	if unauthorized != 5 || throttled != 4 {
		t.Errorf("concurrent guesses: %d unauthorized and %d throttled; want 5 and 4", unauthorized, throttled)
	}
}

// A polling client must never be throttled: valid bearer requests leave the
// limiter untouched, so they neither block nor consume the login budget.
func TestHTTPBearerNotThrottledWhenValid(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)

	for i := 0; i < 35; i++ { // beyond the 30-attempt login cap
		res := doBearer(t, ts, "GET", "/api/jobs", "Bearer "+testAPIToken, nil)
		drain(res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("poll %d = %d", i, res.StatusCode)
		}
	}
	res := e.do("POST", "/api/login", map[string]string{"password": testPassword})
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("login after token burst = %d", res.StatusCode)
	}
}

func TestHTTPBearerDisabledWithoutToken(t *testing.T) {
	e := newHTTPEnv(t, nil, nil) // no API token configured

	// Without API_TOKEN every bearer attempt is a plain 401 — even the
	// value another deployment might run with.
	res := e.bare("GET", "/api/jobs", map[string]string{"Authorization": "Bearer " + testAPIToken})
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bearer while disabled = %d", res.StatusCode)
	}

	// The session flow is unchanged.
	e.login()
	res = e.do("GET", "/api/jobs", nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("session list while disabled = %d", res.StatusCode)
	}
}

func TestHTTPBearerSchemeParsing(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	ts := e.tokenServer(testAPIToken)

	// The scheme is case-insensitive (RFC 6750).
	res := doBearer(t, ts, "GET", "/api/jobs", "bearer "+testAPIToken, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("lowercase scheme = %d", res.StatusCode)
	}

	// Another scheme is not a token attempt: without a session it is the
	// usual cookie 401…
	res = doBearer(t, ts, "GET", "/api/jobs", "Basic dXNlcjpwYXNz", nil)
	var out map[string]string
	decodeJSON(t, res, &out)
	if res.StatusCode != http.StatusUnauthorized || out["error"] != ErrAuth.Error() {
		t.Errorf("basic scheme, no session = %d %v", res.StatusCode, out)
	}
	// …and with a valid session the request goes through despite the
	// stray Authorization header. The test servers share the 127.0.0.1
	// cookie domain, so e.client's session cookie rides along.
	e.login()
	req, _ := http.NewRequest("GET", ts.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	res2, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res2)
	if res2.StatusCode != http.StatusOK {
		t.Errorf("session with basic header = %d", res2.StatusCode)
	}

	// A Bearer challenge with no token still claims the token path: it is a
	// 401 whether or not a session cookie rides along, never a silent
	// fallback to cookie auth. Without a cookie the old code merely drew the
	// session 401; with one (e.client here) the fallback surfaced as a 200.
	res = doBearer(t, ts, "GET", "/api/jobs", "Bearer", nil)
	var noTok map[string]string
	decodeJSON(t, res, &noTok)
	if res.StatusCode != http.StatusUnauthorized || noTok["error"] != "Invalid API token." {
		t.Errorf("bare Bearer without session = %d %v", res.StatusCode, noTok)
	}
	req, _ = http.NewRequest("GET", ts.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer")
	bare, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var bareOut map[string]string
	decodeJSON(t, bare, &bareOut)
	if bare.StatusCode != http.StatusUnauthorized || bareOut["error"] != "Invalid API token." {
		t.Errorf("bare Bearer with session = %d %v", bare.StatusCode, bareOut)
	}

	// "Bearer " with an empty token is the same malformed challenge.
	req, _ = http.NewRequest("GET", ts.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer ")
	empty, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(empty)
	if empty.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty Bearer with session = %d", empty.StatusCode)
	}
}

// Rotating the client-controlled leftmost X-Forwarded-For entry must not
// buy fresh rate-limit buckets: only the suffix the trusted proxy appended
// counts, and that stays fixed here.
func TestHTTPForwardedLeftmostRotationStaysLimited(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	login := func(xff string) int {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/login", strings.NewReader(`{"password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		drain(res)
		return res.StatusCode
	}
	for i := 0; i < 5; i++ {
		if code := login(fmt.Sprintf("198.51.100.%d, 203.0.113.7", i)); code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i, code)
		}
	}
	if code := login("198.51.100.99, 203.0.113.7"); code != http.StatusTooManyRequests {
		t.Fatalf("rotated-leftmost login = %d; want 429", code)
	}
}

func TestClientIPForwardedFor(t *testing.T) {
	loopback, err := ParseTrustedProxies("127.0.0.0/8,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	multiHop, err := ParseTrustedProxies("127.0.0.0/8,::1/128,10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	mappedSpec, err := ParseTrustedProxies("::ffff:172.18.0.2/128")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     string
		want    string
	}{
		// A client-controlled leftmost entry must never become the key.
		{"spoofed leftmost ignored", loopback, "127.0.0.1:4444", "1.2.3.4, 9.9.9.9", "9.9.9.9"},
		{"key follows the rightmost entry", loopback, "127.0.0.1:4444", "1.2.3.4, 8.8.8.8", "8.8.8.8"},
		// Direct peers outside the configured proxies keep their own address.
		{"CGNAT peer ignores XFF", loopback, "100.64.0.7:9999", "1.2.3.4", "100.64.0.7"},
		{"private peer ignores XFF", loopback, "192.168.1.5:8080", "1.2.3.4", "192.168.1.5"},
		{"loopback peer untrusted when list is empty", nil, "127.0.0.1:4444", "9.9.9.9", "127.0.0.1"},
		// A malformed entry can only come from tampering; trust nothing past it.
		{"malformed entry stops the walk", loopback, "127.0.0.1:4444", "9.9.9.9, banana", "127.0.0.1"},
		{"malformed sole entry", loopback, "127.0.0.1:4444", "garbage", "127.0.0.1"},
		// IPv4-mapped IPv6 normalizes to the plain v4 form everywhere.
		{"v4-mapped XFF entry", loopback, "127.0.0.1:4444", "::ffff:203.0.113.9", "203.0.113.9"},
		{"v4-mapped peer", loopback, "[::ffff:127.0.0.1]:4444", "9.9.9.9", "9.9.9.9"},
		{"v4-mapped proxy spec trusts plain peer", mappedSpec, "172.18.0.2:4444", "9.9.9.9", "9.9.9.9"},
		{"v4-mapped proxy spec trusts mapped peer", mappedSpec, "[::ffff:172.18.0.2]:4444", "9.9.9.9", "9.9.9.9"},
		// Multi-hop chains skip every trusted proxy on the way out.
		{"skips trusted middle hop", multiHop, "127.0.0.1:4444", "5.5.5.5, 10.0.0.5", "5.5.5.5"},
		{"all entries trusted falls back to peer", multiHop, "127.0.0.1:4444", "10.0.0.5, 127.0.0.9", "127.0.0.1"},
		// No XFF at all — the proxy itself is the client.
		{"no XFF on trusted peer", loopback, "127.0.0.5:1", "", "127.0.0.5"},
		{"no XFF on IPv6 trusted peer", loopback, "[::1]:4444", "2001:db8::5", "2001:db8::5"},
		// A RemoteAddr that does not parse keys on the raw string.
		{"unparseable RemoteAddr", loopback, "unix-socket", "9.9.9.9", "unix-socket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{trustedProxies: tc.trusted}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := s.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q; want %q", got, tc.want)
			}
		})
	}
}

// Repeated X-Forwarded-For header lines are one chain: an appending proxy
// adds each hop as a new line, so a client-controlled spoof can only sit in
// the earlier lines, never in the appended tail the walk reads first.
func TestClientIPForwardedForRepeatedHeaders(t *testing.T) {
	loopback, err := ParseTrustedProxies("127.0.0.0/8,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"client appended in the second line", []string{"1.2.3.4", "9.9.9.9"}, "9.9.9.9"},
		{"comma lists and lines mix", []string{"1.2.3.4, 8.8.8.8", "9.9.9.9"}, "9.9.9.9"},
		{"walk order extends across lines", []string{"9.9.9.9", "8.8.8.8"}, "8.8.8.8"},
		{"malformed last line falls back to peer", []string{"9.9.9.9", "banana"}, "127.0.0.1"},
		{"malformed earlier line is never reached", []string{"banana", "9.9.9.9"}, "9.9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{trustedProxies: loopback}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "127.0.0.1:4444"
			r.Header["X-Forwarded-For"] = tc.lines
			if got := s.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	cases := []struct {
		spec    string
		want    []netip.Prefix
		wantErr bool
	}{
		{"", nil, false},    // empty trusts nobody
		{"   ", nil, false}, // whitespace only
		{"127.0.0.1", []netip.Prefix{singleAddrPrefix("127.0.0.1")}, false}, // bare IP → /32
		{"2001:db8::1", []netip.Prefix{singleAddrPrefix("2001:db8::1")}, false},
		{"10.0.0.0/8", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, false},
		{"10.1.2.3/8", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, false}, // masked
		{"127.0.0.0/8, ::1/128", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, false},
		{" 10.0.0.5 , 172.16.0.0/12 ", []netip.Prefix{singleAddrPrefix("10.0.0.5"), netip.MustParsePrefix("172.16.0.0/12")}, false},
		// IPv4-mapped entries mean their plain-IPv4 form, matching how peers
		// and XFF entries are normalized before comparison.
		{"::ffff:172.18.0.2", []netip.Prefix{singleAddrPrefix("172.18.0.2")}, false},             // bare mapped IP → plain /32
		{"::ffff:172.18.0.2/128", []netip.Prefix{netip.MustParsePrefix("172.18.0.2/32")}, false}, // mapped CIDR → plain form
		{"::ffff:172.18.0.0/112", []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}, false}, // mapped subnet
		{"::ffff:0:0/96", []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, false},             // whole mapped range ≡ all IPv4
		{"::ffff:172.18.0.0/95", nil, true},                                                      // shorter than the marker cannot translate
		{"banana", nil, true},
		{"10.0.0.0/33", nil, true},
		{"1.2.3.4/99", nil, true},
		{"10.0.0.0/8, banana", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseTrustedProxies(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTrustedProxies(%q) = %v; want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTrustedProxies(%q) error: %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseTrustedProxies(%q) = %v; want %v", tc.spec, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseTrustedProxies(%q)[%d] = %v; want %v", tc.spec, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// singleAddrPrefix builds the /32-or-/128 prefix a bare -trusted-proxies
// entry expands to.
func singleAddrPrefix(ip string) netip.Prefix {
	addr := netip.MustParseAddr(ip)
	return netip.PrefixFrom(addr, addr.BitLen())
}

func TestHTTPLoginCookieFlags(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	server := NewServer(e.manager, e.store, e.auth, e.sessions, e.links, e.limiter, false, defaultTrustedProxies(t), e.assets) // Secure cookies
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", res.StatusCode)
	}
	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("session cookie missing")
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags = %+v", session)
	}
	if session.Path != "/" || session.MaxAge != int(sessionTTL.Seconds()) {
		t.Errorf("cookie scope = %q maxage %d", session.Path, session.MaxAge)
	}

	// The test server serves plain HTTP, so cookies must not be Secure there.
	e2 := newHTTPEnv(t, nil, nil)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req2.Header.Set("Content-Type", "application/json")
	server2 := NewServer(e2.manager, e2.store, e2.auth, e2.sessions, e2.links, e2.limiter, true, defaultTrustedProxies(t), e2.assets)
	server2.Handler().ServeHTTP(rec2, req2)
	res2 := rec2.Result()
	defer res2.Body.Close()
	for _, c := range res2.Cookies() {
		if c.Name == sessionCookie && c.Secure {
			t.Error("Secure cookie over insecure server")
		}
	}
}

func TestHTTPCSRFEnforcement(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()

	// GETs need no CSRF token.
	res := e.do("GET", "/api/session", nil)
	var sess struct {
		CSRFToken string `json:"csrf_token"`
	}
	decodeJSON(t, res, &sess)
	if sess.CSRFToken != e.csrf {
		t.Errorf("session CSRF = %q; want %q", sess.CSRFToken, e.csrf)
	}

	// State changes without a token are rejected.
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(`{"url":"`+goodURL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	res2, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res2)
	if res2.StatusCode != http.StatusForbidden {
		t.Errorf("missing CSRF = %d", res2.StatusCode)
	}

	// Wrong token is rejected.
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(`{"url":"`+goodURL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "forged")
	res3, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res3)
	if res3.StatusCode != http.StatusForbidden {
		t.Errorf("forged CSRF = %d", res3.StatusCode)
	}

	// Cross-site Origin is rejected even with a valid token.
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(`{"url":"`+goodURL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", e.csrf)
	req.Header.Set("Origin", "https://evil.example")
	res4, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res4)
	if res4.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site origin = %d", res4.StatusCode)
	}

	// Same-origin Origin with the token is accepted.
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(`{"url":"`+goodURL+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", e.csrf)
	req.Header.Set("Origin", e.ts.URL)
	res5, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res5)
	if res5.StatusCode != http.StatusAccepted {
		t.Errorf("valid submit = %d", res5.StatusCode)
	}
}

func TestHTTPLogout(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	res := e.do("POST", "/api/logout", nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", res.StatusCode)
	}
	res = e.do("GET", "/api/session", nil)
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("session after logout = %d", res.StatusCode)
	}
}

// Changing PASSWORD and restarting must revoke every stored session: the
// session file is keyed to the boot-time password's fingerprint, so a restart
// under a different password drops all sessions at load time.
func TestHTTPRestartWithNewPasswordRevokesSessions(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	u, err := url.Parse(e.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	stale := ""
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			stale = c.Value
		}
	}
	if stale == "" {
		t.Fatal("session cookie missing after login")
	}

	// A restart under a changed password: a fresh Auth and SessionStore over
	// the same data directory. Loading drops the stored sessions.
	newAuth := NewAuth("fresh-secret", "")
	newSessions := NewSessionStore(filepath.Join(e.dir, "sessions.json"), newAuth.Fingerprint(), e.clk.Now)
	if err := newSessions.Load(); err != nil {
		t.Fatal(err)
	}
	server2 := NewServer(e.manager, e.store, newAuth, newSessions, e.links, e.limiter, true, defaultTrustedProxies(t), e.assets)
	ts2 := httptest.NewServer(server2.Handler())
	defer ts2.Close()

	// The pre-change cookie no longer authenticates.
	req, _ := http.NewRequest("GET", ts2.URL+"/api/session", nil)
	req.Header.Set("Cookie", sessionCookie+"="+stale)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("pre-change session survived restart: %d", res.StatusCode)
	}

	// The old password is refused; the new one works.
	login2 := func(password string) int {
		req, _ := http.NewRequest("POST", ts2.URL+"/api/login", strings.NewReader(`{"password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		drain(res)
		return res.StatusCode
	}
	if code := login2(testPassword); code != http.StatusUnauthorized {
		t.Errorf("stale password accepted: %d", code)
	}
	if code := login2("fresh-secret"); code != http.StatusOK {
		t.Errorf("new password rejected: %d", code)
	}
}

// ---- job submission and validation ----

func TestHTTPSubmitValidation(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()

	// Wrong content type.
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(`{"url":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", e.csrf)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(res)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d", res.StatusCode)
	}

	for name, body := range map[string]string{
		"invalid JSON":   `{"url":`,
		"unknown field":  `{"url":"x","evil":1}`,
		"two objects":    `{"url":"x"}{"url":"y"}`,
		"array body":     `["url"]`,
		"not a URL":      `{"url":"not a url"}`,
		"non-youtube":    `{"url":"https://example.com/watch?v=dQw4w9WgXcQ"}`,
		"playlist":       `{"url":"https://youtube.com/playlist?list=PLx"}`,
		"overlarge body": `{"url":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/jobs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", e.csrf)
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		drain(res)
		want := http.StatusBadRequest
		if name == "overlarge body" {
			want = http.StatusRequestEntityTooLarge
		}
		if res.StatusCode != want {
			t.Errorf("%s = %d; want %d", name, res.StatusCode, want)
		}
	}

	// Bad IDs on the path are rejected too.
	res = e.do("GET", "/api/jobs/not-a-valid-id!", nil)
	drain(res)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id = %d", res.StatusCode)
	}
	res = e.do("GET", "/api/jobs/missingid12", nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job = %d", res.StatusCode)
	}
}

func TestHTTPSubmitAcceptsAndDeduplicates(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()

	id, code := e.submit(t, goodURL)
	if code != http.StatusAccepted {
		t.Errorf("first submit = %d; want 202", code)
	}
	res := e.do("GET", "/api/jobs/"+id, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Errorf("get job = %d", res.StatusCode)
	}

	// The same URL in flight is a duplicate, reported with 200.
	res = e.do("POST", "/api/jobs", map[string]string{"url": goodURL + "&t=30s"})
	var out struct {
		Job       jobView `json:"job"`
		Duplicate bool    `json:"duplicate"`
	}
	decodeJSON(t, res, &out)
	if res.StatusCode != http.StatusOK || !out.Duplicate || out.Job.ID != id {
		t.Errorf("duplicate submit = %d %+v", res.StatusCode, out)
	}

	// Extra query parameters do not defeat dedup.
	e.waitStatus(t, id, StatusReady)
	res = e.do("POST", "/api/jobs", map[string]string{"url": "https://youtu.be/" + id})
	decodeJSON(t, res, &out)
	if res.StatusCode != http.StatusOK || !out.Duplicate {
		t.Errorf("ready duplicate = %d %+v", res.StatusCode, out)
	}
}

// ---- file delivery ----

func TestHTTPDownloadFlow(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)

	video := makeVideo(2000)
	e.store.setFile(id, video)

	// The card reflects the size and the list reports the total.
	res := e.do("GET", "/api/jobs", nil)
	var list struct {
		Jobs       []jobView `json:"jobs"`
		TotalBytes int64     `json:"total_bytes"`
	}
	decodeJSON(t, res, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].Status != StatusReady || list.Jobs[0].Title != "Big Buck Bunny" {
		t.Errorf("list = %+v", list.Jobs)
	}
	// The card size is refreshed from the stored file, matching the total.
	if list.Jobs[0].SizeBytes != int64(len(video)) {
		t.Errorf("job size = %d; want %d (the stored file's size)", list.Jobs[0].SizeBytes, len(video))
	}
	if list.TotalBytes != int64(len(video)) {
		t.Errorf("total = %d; want %d", list.TotalBytes, len(video))
	}

	// Full download.
	res = e.do("GET", "/api/jobs/"+id+"/download", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("content type = %q", ct)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") ||
		!strings.Contains(cd, `filename="Big Buck Bunny.mp4"`) {
		t.Errorf("disposition = %q", cd)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(body, video) {
		t.Errorf("download body = %d bytes; want %d", len(body), len(video))
	}

	// HEAD reports the size without a body.
	res = e.do("HEAD", "/api/jobs/"+id+"/download", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD = %d", res.StatusCode)
	}
	if res.ContentLength != int64(len(video)) {
		t.Errorf("HEAD length = %d; want %d", res.ContentLength, len(video))
	}

	// Range requests seek precisely.
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/jobs/"+id+"/download", nil)
	req.Header.Set("Range", "bytes=10-29")
	ranged, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	partial, _ := io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent {
		t.Fatalf("range = %d", ranged.StatusCode)
	}
	if cr := ranged.Header.Get("Content-Range"); cr != "bytes 10-29/2000" {
		t.Errorf("content-range = %q", cr)
	}
	if !bytes.Equal(partial, video[10:30]) {
		t.Errorf("range body = %d bytes", len(partial))
	}
}

func TestHTTPDownloadLargeFile(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)

	video := makeVideo(5 << 20)
	e.store.setFile(id, video)

	res := e.do("GET", "/api/jobs/"+id+"/download", nil)
	defer res.Body.Close()
	if res.ContentLength != int64(len(video)) {
		t.Errorf("length = %d", res.ContentLength)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, video) {
		t.Errorf("large body = %d bytes; want %d", len(body), len(video))
	}
}

func TestHTTPDownloadUnicodeFilename(t *testing.T) {
	e := newHTTPEnv(t, downloadFunc(func(context.Context, Job, Reporter) (DownloadResult, error) {
		return DownloadResult{Title: "日本語のタイトル 4K", SizeBytes: 100}, nil
	}), nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)
	e.store.setFile(id, makeVideo(100))

	res := e.do("GET", "/api/jobs/"+id+"/download", nil)
	defer res.Body.Close()
	cd := res.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "utf-8''") {
		t.Errorf("unicode disposition = %q", cd)
	}
}

func TestHTTPDownloadNotReadyOrMissing(t *testing.T) {
	// A blocked downloader keeps the job in the downloading state until it
	// is cancelled; afterwards it succeeds for subsequent jobs.
	release := make(chan struct{})
	var cancelled bool
	e := newHTTPEnv(t, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		select {
		case <-release:
			return DownloadResult{Title: "Second Video", SizeBytes: 60}, nil
		case <-ctx.Done():
			cancelled = true
			return DownloadResult{}, ctx.Err()
		}
	}), nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusDownloading)

	res := e.do("GET", "/api/jobs/"+id+"/download", nil)
	drain(res)
	if res.StatusCode != http.StatusConflict {
		t.Errorf("downloading download = %d", res.StatusCode)
	}
	res = e.do("POST", "/api/jobs/"+id+"/link", nil)
	drain(res)
	if res.StatusCode != http.StatusConflict {
		t.Errorf("link while downloading = %d", res.StatusCode)
	}

	// Deletion cancels the pipeline and waits for it.
	res = e.do("DELETE", "/api/jobs/"+id, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete active = %d", res.StatusCode)
	}
	if !cancelled {
		t.Error("delete returned before the pipeline stopped")
	}
	res = e.do("GET", "/api/jobs/"+id, nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("deleted job = %d", res.StatusCode)
	}

	// A ready job whose file vanished serves 404, not a partial.
	close(release)
	id2, _ := e.submit(t, "https://www.youtube.com/watch?v=abcdefghijk")
	e.waitStatus(t, id2, StatusReady)
	res = e.do("GET", "/api/jobs/"+id2+"/download", nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing file download = %d", res.StatusCode)
	}
}

// ---- share links ----

func TestHTTPShareLinkLifecycle(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)
	e.store.setFile(id, makeVideo(3000))

	res := e.do("POST", "/api/jobs/"+id+"/link", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("link issue = %d", res.StatusCode)
	}
	var out struct {
		URL string `json:"url"`
	}
	decodeJSON(t, res, &out)
	// The link ends with the real filename so apps that name a download
	// after the URL's last segment keep the title, not the token.
	if !strings.HasPrefix(out.URL, "/m/") || !strings.HasSuffix(out.URL, "/Big%20Buck%20Bunny.mp4") {
		t.Fatalf("link url = %q", out.URL)
	}

	// No session needed, and the token never grants the API.
	media := e.bare("GET", out.URL, nil)
	defer media.Body.Close()
	if media.StatusCode != http.StatusOK || media.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("share link = %d %s", media.StatusCode, media.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(media.Body)
	if len(body) != 3000 {
		t.Errorf("share body = %d bytes", len(body))
	}
	api := e.bare("GET", "/api/jobs", nil)
	drain(api)
	if api.StatusCode != http.StatusUnauthorized {
		t.Errorf("share token used on API = %d", api.StatusCode)
	}

	// Share links support ranges for in-player seeking.
	ranged := e.bare("GET", out.URL, map[string]string{"Range": "bytes=100-199"})
	partial, _ := io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent || len(partial) != 100 {
		t.Errorf("share range = %d, %d bytes", ranged.StatusCode, len(partial))
	}

	// Token-only links, the shape issued before the filename suffix, still
	// resolve for links that were copied earlier.
	legacy := e.bare("GET", strings.TrimSuffix(out.URL, "/Big%20Buck%20Bunny.mp4"), nil)
	drain(legacy)
	if legacy.StatusCode != http.StatusOK {
		t.Errorf("token-only link = %d", legacy.StatusCode)
	}

	// A link minted against an older incarnation of the same video is dead.
	j := e.waitStatus(t, id, StatusReady)
	stale, _, err := e.links.Issue(id, j.CreatedAt.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res := e.bare("GET", "/m/"+stale, nil); res.StatusCode != http.StatusNotFound {
		drain(res)
		t.Errorf("stale incarnation link = %d", res.StatusCode)
	}

	// Deletion revokes the link immediately.
	res = e.do("DELETE", "/api/jobs/"+id, nil)
	drain(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", res.StatusCode)
	}
	if res := e.bare("GET", out.URL, nil); res.StatusCode != http.StatusNotFound {
		drain(res)
		t.Errorf("revoked link = %d", res.StatusCode)
	}

	// A new incarnation never reactivates the old token.
	id2, code := e.submit(t, goodURL)
	if code != http.StatusAccepted {
		t.Errorf("resubmit after delete = %d", code)
	}
	e.waitStatus(t, id2, StatusReady)
	e.store.setFile(id2, makeVideo(3000))
	if res := e.bare("GET", out.URL, nil); res.StatusCode != http.StatusNotFound {
		drain(res)
		t.Errorf("old token on new incarnation = %d", res.StatusCode)
	}

	// But a freshly issued link for the new incarnation works.
	res = e.do("POST", "/api/jobs/"+id2+"/link", nil)
	decodeJSON(t, res, &out)
	if res := e.bare("GET", out.URL, nil); res.StatusCode != http.StatusOK {
		drain(res)
		t.Errorf("new incarnation link = %d", res.StatusCode)
	}
}

// A title full of path-hostile characters survives into the link: the URL
// ends in one clean filename segment and still serves, while the disposition
// keeps the human-readable name.
func TestHTTPShareLinkFilenameEscaping(t *testing.T) {
	e := newHTTPEnv(t, downloadFunc(func(context.Context, Job, Reporter) (DownloadResult, error) {
		return DownloadResult{Title: `AC/DC: "Let There Be Rock" ✓`, SizeBytes: 1000}, nil
	}), nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)
	e.store.setFile(id, makeVideo(1000))

	res := e.do("POST", "/api/jobs/"+id+"/link", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("link issue = %d", res.StatusCode)
	}
	var out struct {
		URL string `json:"url"`
	}
	decodeJSON(t, res, &out)
	// SafeFilename maps separators and quotes to underscores; PathEscape
	// percent-encodes spaces and non-ASCII (never '+' — that is a literal
	// plus in a path, unlike in a query string).
	const wantSuffix = "/AC_DC_%20_Let%20There%20Be%20Rock_%20%E2%9C%93.mp4"
	if !strings.HasSuffix(out.URL, wantSuffix) {
		t.Fatalf("link url = %q, want suffix %q", out.URL, wantSuffix)
	}

	media := e.bare("GET", out.URL, nil)
	defer media.Body.Close()
	if media.StatusCode != http.StatusOK || media.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("escaped-name link = %d %s", media.StatusCode, media.Header.Get("Content-Type"))
	}
	if disp := media.Header.Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment;") {
		t.Errorf("disposition = %q", disp)
	}
}

func TestHTTPShareLinkExpiry(t *testing.T) {
	e := newHTTPEnv(t, nil, nil)
	e.login()
	id, _ := e.submit(t, goodURL)
	e.waitStatus(t, id, StatusReady)
	e.store.setFile(id, makeVideo(50))

	res := e.do("POST", "/api/jobs/"+id+"/link", nil)
	var out struct {
		URL string `json:"url"`
	}
	decodeJSON(t, res, &out)

	if res := e.bare("GET", out.URL, nil); res.StatusCode != http.StatusOK {
		drain(res)
		t.Fatalf("fresh link = %d", res.StatusCode)
	}
	e.clk.Advance(25 * time.Hour)
	if res := e.bare("GET", out.URL, nil); res.StatusCode != http.StatusNotFound {
		drain(res)
		t.Errorf("expired link = %d", res.StatusCode)
	}
}

// ---- retry ----

func TestHTTPRetryEndpoint(t *testing.T) {
	attempt := 0
	// The fetcher and the downloader agree on the title so the failed card
	// deterministically shows it.
	e := newHTTPEnv(t, downloadFunc(func(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
		attempt++
		if attempt == 1 {
			return DownloadResult{Title: "Flaky Video"}, DownloadError("Unable to download this video. It may be unavailable, private, or age-restricted.")
		}
		return DownloadResult{Title: "Flaky Video", SizeBytes: 777}, nil
	}), fetchFunc(func(_ context.Context, id string) (Metadata, error) {
		return Metadata{ID: id, Title: "Flaky Video", DurationSec: 100, LiveStatus: "not_live"}, nil
	}))
	e.login()
	id, _ := e.submit(t, goodURL)
	failed := e.waitStatus(t, id, StatusFailed)
	if failed.Title != "Flaky Video" || failed.Error == "" {
		t.Errorf("failed card = %+v", failed)
	}

	res := e.do("POST", "/api/jobs/"+id+"/retry", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("retry = %d", res.StatusCode)
	}
	var out struct {
		Job jobView `json:"job"`
	}
	decodeJSON(t, res, &out)
	if out.Job.Status != StatusQueued && out.Job.Status != StatusDownloading {
		t.Errorf("retried status = %q", out.Job.Status)
	}
	e.waitStatus(t, id, StatusReady)

	// Retrying a running job conflicts; unknown and malformed IDs error.
	res = e.do("POST", "/api/jobs/"+id+"/retry", nil)
	drain(res)
	if res.StatusCode != http.StatusConflict {
		t.Errorf("retry ready job = %d", res.StatusCode)
	}
	res = e.do("POST", "/api/jobs/missingid12/retry", nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("retry unknown = %d", res.StatusCode)
	}
	res = e.do("POST", "/api/jobs/not!valid/retry", nil)
	drain(res)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("retry bad id = %d", res.StatusCode)
	}
}
