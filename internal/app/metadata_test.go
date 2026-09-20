package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// lineRunner is a Runner fake that replays fixed output lines and records the
// invocation. The last Run wins in the recorded fields.
type lineRunner struct {
	lines      []string
	err        error
	gotDir     string
	gotProgram string
	gotArgs    []string
}

func (r *lineRunner) Run(_ context.Context, dir, program string, onLine func(string), args ...string) error {
	r.gotDir, r.gotProgram, r.gotArgs = dir, program, append([]string(nil), args...)
	for _, line := range r.lines {
		onLine(line)
	}
	return r.err
}

func fetchWith(lines []string, err error) (YtDLPFetcher, *lineRunner) {
	r := &lineRunner{lines: lines, err: err}
	return YtDLPFetcher{Runner: r, Dir: func(string) string { return "/tmp" }}, r
}

const metaJSON = `{"id":"abcdefghijk","title":"A Title","extractor_key":"Youtube","duration":42.5,"live_status":"not_live"}`

func TestFetchMetadataHappyPath(t *testing.T) {
	// yt-dlp emits warnings and multiple JSON lines; the last one is the video.
	f, r := fetchWith([]string{
		"WARNING: something benign",
		`{"id":"wrong","title":"stale"}`,
		metaJSON,
		"trailing noise",
	}, nil)
	m, err := f.Fetch(context.Background(), "abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "abcdefghijk" || m.Title != "A Title" || m.DurationSec != 42.5 || m.LiveStatus != "not_live" {
		t.Errorf("metadata = %+v", m)
	}
	if r.gotProgram != "yt-dlp" {
		t.Errorf("program = %q", r.gotProgram)
	}
	joined := strings.Join(r.gotArgs, " ")
	for _, want := range []string{
		"--print %(.{id,title,extractor_key,duration,live_status})j",
		"--no-playlist", "--skip-download",
		"-- https://www.youtube.com/watch?v=abcdefghijk",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %q", want, joined)
		}
	}
	if r.gotArgs[len(r.gotArgs)-2] != "--" {
		t.Errorf("URL not protected by --: %v", r.gotArgs)
	}
}

func TestFetchMetadataAcceptsFormerLivestream(t *testing.T) {
	payload := `{"id":"abcdefghijk","title":"VOD","extractor_key":"Youtube","duration":600,"live_status":"was_live"}`
	f, _ := fetchWith([]string{payload}, nil)
	m, err := f.Fetch(context.Background(), "abcdefghijk")
	if err != nil || m.LiveStatus != "was_live" || m.Title != "VOD" {
		t.Errorf("was_live rejected: %+v, %v", m, err)
	}
}

func TestFetchMetadataRejectsLiveAndUpcoming(t *testing.T) {
	for _, status := range []string{"is_live", "post_live", "is_upcoming", ""} {
		payload := `{"id":"abcdefghijk","title":"T","extractor_key":"Youtube","duration":0,"live_status":"` + status + `"}`
		f, _ := fetchWith([]string{payload}, nil)
		if _, err := f.Fetch(context.Background(), "abcdefghijk"); err == nil ||
			err.Error() != "Live and upcoming streams are not supported." {
			t.Errorf("live_status %q: err = %v", status, err)
		}
	}
}

func TestFetchMetadataRejectsWrongIdentity(t *testing.T) {
	for _, payload := range []string{
		`{"id":"somethinels","title":"T","extractor_key":"Youtube","duration":1,"live_status":"not_live"}`,
		`{"id":"abcdefghijk","title":"T","extractor_key":"Vimeo","duration":1,"live_status":"not_live"}`,
		`{"id":"abcdefghijk","title":"T","extractor_key":"Generic","duration":1,"live_status":"not_live"}`,
		`{"id":"","title":"T","extractor_key":"Youtube","duration":1,"live_status":"not_live"}`,
	} {
		f, _ := fetchWith([]string{payload}, nil)
		if _, err := f.Fetch(context.Background(), "abcdefghijk"); err == nil ||
			err.Error() != "YouTube returned invalid video metadata." {
			t.Errorf("payload %s: err = %v", payload, err)
		}
	}
}

func TestFetchMetadataNoJSONLine(t *testing.T) {
	for _, lines := range [][]string{
		nil,
		{"[youtube] abcdefghijk: Downloading webpage"},
		{"ERROR: unable to download video"},
	} {
		f, _ := fetchWith(lines, nil)
		if _, err := f.Fetch(context.Background(), "abcdefghijk"); err == nil ||
			err.Error() != "YouTube returned no video metadata." {
			t.Errorf("lines %v: err = %v", lines, err)
		}
	}
}

func TestFetchMetadataRejectsMalformedJSON(t *testing.T) {
	f, _ := fetchWith([]string{`{"id":"abcdefghijk","title":`}, nil)
	if _, err := f.Fetch(context.Background(), "abcdefghijk"); err == nil ||
		err.Error() != "YouTube returned invalid video metadata." {
		t.Errorf("malformed JSON: err = %v", err)
	}
}

func TestFetchMetadataRunnerErrorPassthrough(t *testing.T) {
	boom := errors.New("spawn failed")
	f, _ := fetchWith(nil, boom)
	if _, err := f.Fetch(context.Background(), "abcdefghijk"); !errors.Is(err, boom) {
		t.Errorf("runner error swallowed: %v", err)
	}
}

func TestFetchMetadataCancellationWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f, _ := fetchWith(nil, context.Canceled)
	if _, err := f.Fetch(ctx, "abcdefghijk"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled fetch = %v; want context.Canceled", err)
	}
}
