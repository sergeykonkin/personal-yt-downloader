package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"personal-yt-downloader/internal/app"
)

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  server [flags]         Run the downloader service
  server healthcheck     Probe the running service

Flags for server:
  -data DIR              data directory (default "/data")
  -addr ADDR             listen address (default ":8080")
  -insecure-cookies      allow session cookies over plain HTTP (local dev)
  -trusted-proxies LIST  comma-separated IPs/CIDRs whose X-Forwarded-For is
                         trusted for rate limiting
                         (default "127.0.0.0/8,::1/128"; empty trusts none)

The login password comes from the PASSWORD environment variable. Setting
API_TOKEN (optional) additionally enables "Authorization: Bearer <token>"
authentication for direct API requests; unset or empty disables it.
`)
}

func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			return healthcheck()
		case "-h", "--help", "help":
			usage()
			return nil
		}
		if !strings.HasPrefix(os.Args[1], "-") {
			usage()
			return fmt.Errorf("unknown command %q", os.Args[1])
		}
	}
	return serve(os.Args[1:])
}

func healthcheck() error {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		return errors.New("The service is unavailable.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("The service is unhealthy.")
	}
	return nil
}

// serve reads two environment variables — the login password and the
// optional API token for Bearer authentication — and takes everything else
// as explicit flags.
func serve(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	dataDir := fs.String("data", "/data", "data directory")
	addr := fs.String("addr", ":8080", "listen address")
	insecure := fs.Bool("insecure-cookies", false, "allow session cookies over plain HTTP")
	trustedProxiesSpec := fs.String("trusted-proxies", "127.0.0.0/8,::1/128",
		"comma-separated IPs/CIDRs whose X-Forwarded-For is trusted for rate limiting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	trustedProxies, err := app.ParseTrustedProxies(*trustedProxiesSpec)
	if err != nil {
		return err
	}
	root := *dataDir
	if err := os.MkdirAll(root, 0o700); err != nil {
		return errors.New("Unable to create the data directory.")
	}
	password := os.Getenv("PASSWORD")
	if password == "" {
		return errors.New("PASSWORD must be set to a non-empty password.")
	}
	if utf8.RuneCountInString(password) > 1024 {
		return errors.New("PASSWORD is too long (over 1024 characters).")
	}
	// Trimmed to match what a client can present: the token pulled out of
	// an Authorization header never carries surrounding whitespace, so a
	// padded env var would otherwise be an unusable token.
	apiToken := strings.TrimSpace(os.Getenv("API_TOKEN"))
	if utf8.RuneCountInString(apiToken) > 1024 {
		return errors.New("API_TOKEN is too long (over 1024 characters).")
	}

	runner := app.ProcessRunner{}
	store := app.DiskStore{Root: root}
	fetcher := app.YtDLPFetcher{Runner: runner, Dir: func(id string) string {
		return filepath.Join(root, "videos", id)
	}}
	downloader := app.VideoDownloader{Runner: runner, Root: root, Fetch: fetcher}

	app.CleanWorkFiles(root)
	manager := app.NewManager(store, downloader, fetcher)
	defer manager.Close()
	if err := manager.Recover(); err != nil {
		return errors.New("Unable to read the stored jobs: " + err.Error())
	}

	auth := app.NewAuth(password, apiToken)
	if apiToken != "" {
		slog.Info("Bearer token authentication enabled for API requests")
	}
	sessions := app.NewSessionStore(filepath.Join(root, "sessions.json"), auth.Fingerprint(), nil)
	if err := sessions.Load(); err != nil {
		slog.Error("Sessions could not be loaded; all sessions were dropped", "error", err.Error())
	}
	links := app.NewLinkStore(filepath.Join(root, "links.json"), nil)
	if err := links.Load(); err != nil {
		slog.Error("Share links could not be loaded; existing links were dropped", "error", err.Error())
	}
	limiter := app.NewRateLimiter(15*time.Minute, 5, 30, nil)
	server := app.NewServer(manager, store, auth, sessions, links, limiter, *insecure, trustedProxies, app.FrontendFS())

	stopPruning := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stopPruning:
				return
			case <-ticker.C:
				sessions.Prune()
				links.Prune()
			}
		}
	}()

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      time.Hour, // large downloads may take a while
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		slog.Info("Service listening", "address", addr, "go", runtime.Version())
		done <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-done:
		close(stopPruning)
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		manager.Close()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
		}
		close(stopPruning)
	}
	return nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("Service failed", "error", err.Error())
		os.Exit(1)
	}
}
