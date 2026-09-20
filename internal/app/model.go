package app

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Job status values, persisted in metadata and reported to the client.
const (
	StatusQueued      = "queued"
	StatusDownloading = "downloading"
	StatusProcessing  = "processing"
	StatusVerifying   = "verifying"
	StatusReady       = "ready"
	StatusFailed      = "failed"
)

// Stage refines the status for display. Download stages distinguish the audio
// and video streams; the JSON field is omitted when it adds nothing.
const (
	StageQueued        = "queued"
	StageDownloadVideo = "downloading.video"
	StageDownloadAudio = "downloading.audio"
	StageProcessing    = "processing"
	StageVerifying     = "verifying"
	StageReady         = "ready"
	StageFailed        = "failed"
)

func statusForStage(stage string) string {
	switch stage {
	case StageDownloadVideo, StageDownloadAudio:
		return StatusDownloading
	case StageProcessing:
		return StatusProcessing
	case StageVerifying:
		return StatusVerifying
	case StageReady:
		return StatusReady
	case StageFailed:
		return StatusFailed
	default:
		return StatusQueued
	}
}

var (
	ErrNotFound = errors.New("Job not found.")
	ErrBusy     = errors.New("The download queue is full. Try again later.")
	ErrStorage  = errors.New("Storage is unavailable or full.")
	ErrClosed   = errors.New("The service is shutting down. Try again later.")
	ErrAuth     = errors.New("A valid session is required.")
	ErrConflict = errors.New("The request conflicts with the current job state.")
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

// Job is the persistent unit of work. Title, duration, and status survive
// restarts; progress is volatile by design.
type Job struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	DurationSec float64   `json:"duration_sec,omitempty"`
	Status      string    `json:"status"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Error       string    `json:"error,omitempty"`
}

// unfinished reports whether the job still counts toward the admission limit.
func (j Job) unfinished() bool {
	switch j.Status {
	case StatusQueued, StatusDownloading, StatusProcessing, StatusVerifying:
		return true
	default:
		return false
	}
}

// jobView is the JSON representation served to the frontend.
type jobView struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage"`
	Progress  *float64  `json:"progress,omitempty"` // omitted when the total is unknown
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`
}

func view(j Job, stage string, progress float64, known bool) jobView {
	v := jobView{ID: j.ID, Title: j.Title, Status: j.Status, Stage: stage, SizeBytes: j.SizeBytes, CreatedAt: j.CreatedAt.UTC()}
	if v.Title == "" {
		v.Title = "Fetching title…" // replaced by the client for failed jobs without a title
	}
	if v.Stage == "" {
		v.Stage = v.Status
	}
	if known {
		p := progress
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		v.Progress = &p
	}
	if j.Status == StatusFailed {
		v.Error = j.Error
	}
	return v
}

// VideoID accepts explicit single-video URL forms and discards tracking
// parameters. Playlists, and anything that is not one video, are rejected.
// The downloader re-verifies the ID and extractor against YouTube metadata.
func VideoID(raw string) (string, error) {
	bad := errors.New("Provide a valid YouTube watch, Shorts, live, embed, or youtu.be URL.")
	if len(raw) > 4096 {
		return "", bad
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return "", bad
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimSuffix(u.Path, "/")
	var id string
	switch host {
	case "youtu.be":
		id = strings.TrimPrefix(path, "/")
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		if path == "/watch" {
			values, e := url.ParseQuery(u.RawQuery)
			if e != nil || len(values["v"]) != 1 {
				return "", bad
			}
			id = values.Get("v")
		} else {
			pieces := strings.Split(path, "/")
			if len(pieces) != 3 || (pieces[1] != "shorts" && pieces[1] != "live" && pieces[1] != "embed") {
				return "", bad
			}
			id = pieces[2]
		}
	default:
		return "", bad
	}
	if !idPattern.MatchString(id) {
		return "", bad
	}
	return id, nil
}

// SafeFilename derives a download filename from the video title. Control
// characters, format modifiers, separators, and quotes are replaced; length is
// bounded by runes; the result always ends in .mp4.
func SafeFilename(title, id string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, title)
	title = strings.Trim(title, " .\t\n\r")
	for len(title) > 180 {
		_, n := utf8.DecodeLastRuneInString(title)
		title = title[:len(title)-n]
	}
	title = strings.Trim(title, " .")
	if title == "" {
		title = id
	}
	return title + ".mp4"
}
