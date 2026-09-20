package app

import (
	"context"
	"encoding/json"
	"strings"
)

// Metadata is the subset of YouTube metadata the pipeline needs. Titles are
// fetched independently of the download queue so they appear early.
type Metadata struct {
	ID          string
	Title       string
	DurationSec float64
	LiveStatus  string
}

// MetadataFetcher retrieves metadata for one video without downloading it.
type MetadataFetcher interface {
	Fetch(ctx context.Context, id string) (Metadata, error)
}

// YtDLPFetcher runs yt-dlp once per video. Bounded network retries are
// handled by yt-dlp's own flags.
type YtDLPFetcher struct {
	Runner Runner
	// Dir is the working directory for the probe. It only needs to exist.
	Dir func(id string) string
}

func (f YtDLPFetcher) Fetch(ctx context.Context, id string) (Metadata, error) {
	args := append(ytDLPBase(),
		"--no-progress", "--skip-download",
		"--print", "%(.{id,title,extractor_key,duration,live_status})j",
		"--", "https://www.youtube.com/watch?v="+id)
	var lines []string
	err := f.Runner.Run(ctx, f.Dir(id), "yt-dlp", func(line string) { lines = append(lines, line) }, args...)
	if err != nil {
		if ctx.Err() != nil {
			return Metadata{}, ctx.Err()
		}
		return Metadata{}, err
	}
	payload := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "{") {
			payload = line
		}
	}
	if payload == "" {
		return Metadata{}, DownloadError("YouTube returned no video metadata.")
	}
	var info struct {
		ID         string  `json:"id"`
		Title      string  `json:"title"`
		Extractor  string  `json:"extractor_key"`
		Duration   float64 `json:"duration"`
		LiveStatus string  `json:"live_status"`
	}
	if json.Unmarshal([]byte(payload), &info) != nil || info.ID != id || info.Extractor != "Youtube" {
		return Metadata{}, DownloadError("YouTube returned invalid video metadata.")
	}
	switch info.LiveStatus {
	case "not_live", "was_live":
	default:
		return Metadata{}, DownloadError("Live and upcoming streams are not supported.")
	}
	return Metadata{ID: info.ID, Title: info.Title, DurationSec: info.Duration, LiveStatus: info.LiveStatus}, nil
}

func ytDLPBase() []string {
	return []string{
		"--ignore-config", "--no-plugin-dirs", "--no-playlist", "--no-warnings",
		"--no-cache-dir", "--js-runtimes", "node", "--no-remote-components",
		"--socket-timeout", "20", "--retries", "3", "--fragment-retries", "3",
		"--extractor-retries", "2", "--use-extractors", "youtube", "--newline",
	}
}
