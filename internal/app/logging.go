package app

import (
	"crypto/rand"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type responseLog struct {
	http.ResponseWriter
	status      int
	bytes       int64
	writeFailed bool
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseLog) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseLog) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	w.writeFailed = w.writeFailed || err != nil
	return n, err
}

// requestLogging logs route categories and validated job IDs — never
// headers, bodies, query strings, session cookies, or share tokens.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := rand.Text()
		w.Header().Set("X-Request-Id", id)
		route, jobID := categorize(r)
		method := r.Method
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		default:
			method = "OTHER"
		}
		attrs := []any{"request_id", id, "method", method, "route", route}
		if jobID != "" {
			attrs = append(attrs, "id", jobID)
		}
		quiet := route == "/healthz"
		if !quiet {
			slog.Info("HTTP request started", attrs...)
		}
		tracked := &responseLog{ResponseWriter: w}
		defer func() {
			status := tracked.status
			if status == 0 {
				status = http.StatusOK
			}
			done := append(attrs, "status", status, "bytes", tracked.bytes, "duration_ms", time.Since(started).Milliseconds(), "write_failed", tracked.writeFailed, "canceled", r.Context().Err() != nil)
			if status >= 400 || tracked.writeFailed {
				slog.Warn("HTTP request completed", done...)
			} else if !quiet {
				slog.Info("HTTP request completed", done...)
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

// categorize maps a request path to a route label. Share tokens are never
// logged: /m/<secret> is recorded only as "/m/{token}".
func categorize(r *http.Request) (route, jobID string) {
	path := r.URL.Path
	switch path {
	case "/healthz":
		return "/healthz", ""
	case "/api/login":
		return "/api/login", ""
	case "/api/session":
		return "/api/session", ""
	case "/api/logout":
		return "/api/logout", ""
	case "/api/jobs":
		return "/api/jobs", ""
	case "/":
		return "/", ""
	}
	if strings.HasPrefix(path, "/assets/") {
		return "/assets", ""
	}
	if strings.HasPrefix(path, "/m/") {
		return "/m/{token}", ""
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[1] == "api" && parts[2] == "jobs" && idPattern.MatchString(parts[3]) {
		return "/api/jobs/{id}", parts[3]
	}
	if len(parts) == 5 && parts[1] == "api" && parts[2] == "jobs" && idPattern.MatchString(parts[3]) {
		switch parts[4] {
		case "retry", "link", "download":
			return "/api/jobs/{id}/" + parts[4], parts[3]
		}
	}
	return "unknown", ""
}
