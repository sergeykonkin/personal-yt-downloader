package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// ---- scripted runner ----

// scriptStep describes one expected command: the program it must be, argument
// substrings it must contain, files to create in the working directory before
// emitting lines (the pipeline stats real files), and the error to return.
type scriptStep struct {
	program string
	need    []string
	lines   []string
	files   map[string]int64
	err     error
}

// scriptRunner replays a scripted command sequence. Unexpected commands,
// wrong programs, or missing argument substrings fail the run loudly, so
// call-order regressions surface as pipeline errors.
type scriptRunner struct {
	mu    sync.Mutex
	steps []scriptStep
	calls []string
}

func (r *scriptRunner) queue(steps ...scriptStep) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, steps...)
}

func (r *scriptRunner) Run(_ context.Context, dir, program string, onLine func(string), args ...string) error {
	r.mu.Lock()
	if len(r.steps) == 0 {
		remaining := fmt.Sprintf("no steps left for %s %v", program, args)
		r.mu.Unlock()
		return fmt.Errorf("scriptRunner: %s", remaining)
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	r.calls = append(r.calls, program+" "+strings.Join(args, " "))
	r.mu.Unlock()

	if step.program != "" && step.program != program {
		return fmt.Errorf("scriptRunner: expected %s but got %s (args %v)", step.program, program, args)
	}
	joined := strings.Join(args, " ")
	for _, sub := range step.need {
		if !strings.Contains(joined, sub) {
			return fmt.Errorf("scriptRunner: %s args missing %q: %v", program, sub, args)
		}
	}
	for name, size := range step.files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			return err
		}
	}
	for _, line := range step.lines {
		onLine(line)
	}
	return step.err
}

func (r *scriptRunner) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// ---- ffprobe fixtures ----

const (
	probeH264Landscape = `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720}],"format":{"duration":"60.5"}}`
	probeH264MuxedAAC  = `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"60.5"}}`
	probeAACAudioOnly  = `{"streams":[{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"60.5"}}`
	probeOpusAudioOnly = `{"streams":[{"codec_type":"audio","codec_name":"opus"}],"format":{"duration":"60.5"}}`
	probeVP9Portrait   = `{"streams":[{"codec_type":"video","codec_name":"vp9","pix_fmt":"yuv420p","width":1080,"height":1920}],"format":{"duration":"60.5"}}`
	probeFinalGood     = `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"60.4"}}`
	probeFinalHEVC     = `{"streams":[{"codec_type":"video","codec_name":"hevc","pix_fmt":"yuv420p","width":1280,"height":720},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"60.4"}}`
	probeFinalShort    = `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"10.0"}}`
	probeAudioMissing  = `{"streams":[],"format":{"duration":"60.5"}}`
)

// downloadVideoStep and friends script the standard pipeline commands.
func downloadVideoStep(print string, size int64) scriptStep {
	return scriptStep{
		program: "yt-dlp",
		need:    []string{"--format " + videoSelector, "video.%(ext)s", "--match-filters", "--print after_move:filepath", "--progress --progress-template download:[download] %(progress._percent_str)s", "--progress-delta 1"},
		files:   map[string]int64{print: size},
		lines:   []string{"[download]  10.0% of ~1MiB", "[download] 100.0% of 1MiB", print},
	}
}

func probeStep(path, payload string) scriptStep {
	return scriptStep{program: "ffprobe", need: []string{path}, lines: []string{payload}}
}

func ffmpegStep(need []string, lines ...string) scriptStep {
	all := append([]string{"-movflags +faststart"}, need...)
	return scriptStep{program: "ffmpeg", need: all, files: map[string]int64{"output.mp4": 4096}, lines: lines}
}

type reportRecord struct {
	stage    string
	progress float64
	known    bool
}

func recordReports(out *[]reportRecord) Reporter {
	return func(stage string, progress float64, known bool) {
		*out = append(*out, reportRecord{stage, progress, known})
	}
}

func newDownloadEnv(t *testing.T) (VideoDownloader, *scriptRunner, string) {
	t.Helper()
	root := t.TempDir()
	runner := &scriptRunner{}
	// The default fetcher fails loudly: any test that unexpectedly needs a
	// title fetch breaks with a clear error instead of silently passing.
	d := VideoDownloader{
		Runner: runner,
		Root:   root,
		Fetch: fetchFunc(func(context.Context, string) (Metadata, error) {
			return Metadata{}, DownloadError("unexpected metadata fetch")
		}),
	}
	return d, runner, root
}

func assertPublished(t *testing.T, root, id string, wantSize int64) {
	t.Helper()
	published := filepath.Join(root, "videos", id, "video.mp4")
	info, err := os.Stat(published)
	if err != nil {
		t.Fatalf("video not published: %v", err)
	}
	if info.Size() != wantSize {
		t.Errorf("published size = %d; want %d", info.Size(), wantSize)
	}
	if work, err := os.Stat(filepath.Join(root, "videos", id, "work")); err == nil {
		t.Errorf("work dir survived: %v", work)
	}
}

func assertNothingPublished(t *testing.T, root, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "videos", id, "video.mp4")); !os.IsNotExist(err) {
		t.Errorf("video published despite failure: %v", err)
	}
	if work, err := os.Stat(filepath.Join(root, "videos", id, "work")); err == nil {
		t.Errorf("work dir survived failure: %v", work)
	}
}

// ---- pipeline flow tests ----

func TestDownloadRemuxesCompatibleSource(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(1)
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeH264MuxedAAC),
		ffmpegStep([]string{"-c copy"}, "out_time_us=10000000", "progress=continue", "out_time_us=60500000", "progress=end"),
		probeStep("output.mp4", probeFinalGood),
	)

	var reports []reportRecord
	res, err := d.Download(context.Background(), Job{ID: id, Title: "Muxed Video", DurationSec: 62.5}, recordReports(&reports))
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Muxed Video" || res.SizeBytes != 4096 || res.DurationSec != 62.5 {
		t.Errorf("result = %+v", res)
	}
	assertPublished(t, root, id, 4096)

	calls := runner.recorded()
	if len(calls) != 4 {
		t.Fatalf("calls = %d; want 4:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[0], "--format "+videoSelector) {
		t.Errorf("video selector missing: %s", calls[0])
	}
	if strings.Contains(calls[0], audioSelector) {
		t.Errorf("audio selector used on video download: %s", calls[0])
	}
	ffmpeg := calls[2]
	for _, want := range []string{"-c copy", "-movflags +faststart", "-map_metadata -1", "-sn"} {
		if !strings.Contains(ffmpeg, want) {
			t.Errorf("ffmpeg args missing %q: %s", want, ffmpeg)
		}
	}
	if n := strings.Count(ffmpeg, " -i "); n != 1 {
		t.Errorf("remux inputs = %d; want 1: %s", n, ffmpeg)
	}

	wantReports := []reportRecord{
		{StageDownloadVideo, 0, false},
		{StageDownloadVideo, 10, true},
		{StageDownloadVideo, 100, true},
		{StageProcessing, 0, true},
		{StageProcessing, 0, true},
		{StageProcessing, 10000000 / (60.5 * 1e6) * 100, true},
		{StageProcessing, 100, true},
		{StageProcessing, 100, true}, // out_time_us == duration, then progress=end
		{StageVerifying, 0, false},
	}
	if len(reports) != len(wantReports) {
		t.Fatalf("reports = %+v; want %+v", reports, wantReports)
	}
	for i, want := range wantReports {
		got := reports[i]
		if got.stage != want.stage || got.known != want.known {
			t.Errorf("report[%d] = %+v; want %+v", i, got, want)
		}
		if diff := got.progress - want.progress; diff < -0.01 || diff > 0.01 {
			t.Errorf("report[%d] progress = %v; want %v", i, got.progress, want.progress)
		}
	}
}

func TestDownloadCopiesVideoAndEncodesAudio(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(2)
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeH264Landscape), // video-only: no audio stream
		scriptStep{
			program: "yt-dlp",
			need:    []string{"--format " + audioSelector, "audio.%(ext)s"},
			files:   map[string]int64{"audio.webm": 512},
			lines:   []string{"[download]  50.0% of ~1MiB", "audio.webm"},
		},
		probeStep("audio.webm", probeOpusAudioOnly),
		ffmpegStep([]string{"-c:v copy", "-c:a aac", "-b:a 128k", "audio.webm"}, "progress=end"),
		probeStep("output.mp4", probeFinalGood),
	)

	res, err := d.Download(context.Background(), Job{ID: id, Title: "Separate Audio"}, func(string, float64, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Separate Audio" || res.SizeBytes != 4096 {
		t.Errorf("result = %+v", res)
	}
	assertPublished(t, root, id, 4096)

	calls := runner.recorded()
	if len(calls) != 6 {
		t.Fatalf("calls = %d; want 6:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[2], audioSelector) {
		t.Errorf("audio download without audio selector: %s", calls[2])
	}
	ffmpeg := calls[4]
	if !strings.Contains(ffmpeg, "-map 1:a:0") || strings.Contains(ffmpeg, "libx264") {
		t.Errorf("unexpected ffmpeg remap: %s", ffmpeg)
	}
	if n := strings.Count(ffmpeg, " -i "); n != 2 {
		t.Errorf("ffmpeg inputs = %d; want 2: %s", n, ffmpeg)
	}
}

func TestDownloadTranscodesIncompatibleSource(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(3)
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeVP9Portrait), // vp9 portrait: out of bounds
		scriptStep{
			program: "yt-dlp",
			need:    []string{"--format " + audioSelector},
			files:   map[string]int64{"audio.m4a": 512},
			lines:   []string{"[download] 100.0% of 1MiB", "audio.m4a"},
		},
		probeStep("audio.m4a", probeAACAudioOnly),
		ffmpegStep([]string{"-c:v libx264", "-preset veryfast", "-pix_fmt yuv420p", scaleFilter(1080, 1920), "-c:a copy"}, "progress=end"),
		probeStep("output.mp4", probeFinalGood),
	)

	res, err := d.Download(context.Background(), Job{ID: id, Title: "VP9 Portrait"}, func(string, float64, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "VP9 Portrait" || res.SizeBytes != 4096 {
		t.Errorf("result = %+v", res)
	}
	assertPublished(t, root, id, 4096)

	calls := runner.recorded()
	if len(calls) != 6 {
		t.Fatalf("calls = %d; want 6:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	ffmpeg := calls[4]
	if !strings.Contains(ffmpeg, scaleFilter(1080, 1920)) {
		t.Errorf("portrait scale filter missing: %s", ffmpeg)
	}
	if strings.Contains(ffmpeg, "-c:v copy") {
		t.Errorf("incompatible video copied: %s", ffmpeg)
	}
}

func TestDownloadAcceptsPortraitWithinBounds(t *testing.T) {
	// A 720×1280 h264+aac source fits the ceiling without transcoding.
	d, runner, root := newDownloadEnv(t)
	id := videoID(4)
	portrait := `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":720,"height":1280},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"60.5"}}`
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", portrait),
		ffmpegStep([]string{"-c copy"}, "progress=end"),
		probeStep("output.mp4", probeFinalGood),
	)
	if _, err := d.Download(context.Background(), Job{ID: id, Title: "Portrait"}, func(string, float64, bool) {}); err != nil {
		t.Fatal(err)
	}
	assertPublished(t, root, id, 4096)
	if calls := runner.recorded(); len(calls) != 4 {
		t.Errorf("calls = %d; want 4: %s", len(calls), strings.Join(calls, "\n"))
	}
}

func TestDownloadFailsOnBadOutput(t *testing.T) {
	cases := []struct {
		name      string
		finalJSON string
		wantErr   string
	}{
		{"wrong final codec", probeFinalHEVC, "Unable to produce a compatible MP4."},
		{"truncated final duration", probeFinalShort, "The prepared file is incomplete."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, runner, root := newDownloadEnv(t)
			id := videoID(5)
			runner.queue(
				downloadVideoStep("video.mp4", 2048),
				probeStep("video.mp4", probeH264MuxedAAC),
				ffmpegStep([]string{"-c copy"}, "progress=end"),
				probeStep("output.mp4", c.finalJSON),
			)
			res, err := d.Download(context.Background(), Job{ID: id, Title: "Doomed"}, func(string, float64, bool) {})
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("err = %v; want %q", err, c.wantErr)
			}
			if res.Title != "Doomed" {
				t.Errorf("title lost on failure: %+v", res)
			}
			assertNothingPublished(t, root, id)
		})
	}
}

func TestDownloadRejectsAudiolessOutput(t *testing.T) {
	// Source video has no audio and the audio download yields nothing either.
	d, runner, root := newDownloadEnv(t)
	id := videoID(6)
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeH264Landscape),
		scriptStep{program: "yt-dlp", need: []string{audioSelector}, lines: []string{"[download] 100.0% of 1MiB"}},
	)
	if _, err := d.Download(context.Background(), Job{ID: id, Title: "Silent"}, func(string, float64, bool) {}); err == nil ||
		err.Error() != "No complete stream was downloaded." {
		t.Errorf("err = %v", err)
	}
	assertNothingPublished(t, root, id)
}

func TestDownloadRejectsStreamlessSource(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(7)
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeAudioMissing),
	)
	if _, err := d.Download(context.Background(), Job{ID: id, Title: "No Video"}, func(string, float64, bool) {}); err == nil ||
		err.Error() != "The download does not contain a video stream." {
		t.Errorf("err = %v", err)
	}
	assertNothingPublished(t, root, id)
}

func TestDownloadRejectsUnsafeYtDLPPaths(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		files map[string]int64
	}{
		{"parent directory escape", []string{"[download] 100.0% of 1MiB", "../evil.mp4"}, map[string]int64{"../evil.mp4": 100}},
		{"absolute path elsewhere", []string{"/etc/passwd"}, nil},
		{"wrong file prefix", []string{"clip.mp4"}, map[string]int64{"clip.mp4": 5}},
		{"no printed path", []string{"[download] 100.0% of 1MiB"}, nil},
		{"empty file", []string{"video.mp4"}, map[string]int64{"video.mp4": 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, runner, root := newDownloadEnv(t)
			id := videoID(8)
			runner.queue(scriptStep{program: "yt-dlp", need: []string{"video.%(ext)s"}, lines: c.lines, files: c.files})
			if _, err := d.Download(context.Background(), Job{ID: id, Title: "T"}, func(string, float64, bool) {}); err == nil ||
				err.Error() != "No complete stream was downloaded." {
				t.Errorf("err = %v", err)
			}
			assertNothingPublished(t, root, id)
		})
	}
}

func TestDownloadFetchesTitleWhenMissing(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(9)
	fetches := 0
	d.Fetch = fetchFunc(func(context.Context, string) (Metadata, error) {
		fetches++
		return Metadata{ID: id, Title: "Fetched Title", DurationSec: 61, LiveStatus: "not_live"}, nil
	})
	runner.queue(
		downloadVideoStep("video.mp4", 2048),
		probeStep("video.mp4", probeH264MuxedAAC),
		ffmpegStep([]string{"-c copy"}, "progress=end"),
		probeStep("output.mp4", probeFinalGood),
	)
	res, err := d.Download(context.Background(), Job{ID: id}, func(string, float64, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Errorf("fetches = %d; want 1", fetches)
	}
	if res.Title != "Fetched Title" || res.DurationSec != 61 {
		t.Errorf("result = %+v", res)
	}
	assertPublished(t, root, id, 4096)
}

func TestDownloadFetchFailureShortCircuits(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(10)
	d.Fetch = fetchFunc(func(context.Context, string) (Metadata, error) {
		return Metadata{}, DownloadError("Live and upcoming streams are not supported.")
	})
	res, err := d.Download(context.Background(), Job{ID: id}, func(string, float64, bool) {})
	if err == nil || err.Error() != "Live and upcoming streams are not supported." {
		t.Fatalf("err = %v", err)
	}
	if res.Title != "" || res.SizeBytes != 0 {
		t.Errorf("result = %+v", res)
	}
	if calls := runner.recorded(); len(calls) != 0 {
		t.Errorf("pipeline ran after fetch failure: %s", strings.Join(calls, "\n"))
	}
	assertNothingPublished(t, root, id)
}

func TestDownloadCancellationCleansUp(t *testing.T) {
	d, runner, root := newDownloadEnv(t)
	id := videoID(11)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner.queue(scriptStep{program: "yt-dlp", err: context.Canceled})
	res, err := d.Download(ctx, Job{ID: id, Title: "Cancelled"}, func(string, float64, bool) {})
	if err != context.Canceled {
		t.Errorf("err = %v; want context.Canceled", err)
	}
	if res.Title != "Cancelled" {
		t.Errorf("title lost on cancellation: %+v", res)
	}
	assertNothingPublished(t, root, id)
}

// ---- unit tests ----

func TestParseDownloadPercent(t *testing.T) {
	cases := []struct {
		line string
		want float64
		ok   bool
	}{
		{"[download]  42.7% of ~10.50MiB at 1 MiB/s ETA 00:08", 42.7, true},
		{"[download] 100.0% of 1MiB in 00:01 at 2 MiB/s", 100, true},
		{"[download]   0.0% of ~0B at Unknown B/s ETA unknown", 0, true},
		{"[download]  42.7%", 42.7, true},
		{"[download] 101% of x", 0, false},
		{"[download] -1% of x", 0, false},
		{"[download] of ~10MiB", 0, false},
		{"[download] abc% of x", 0, false},
		{"42.7% of x", 0, false},
		{"[download]", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDownloadPercent(c.line)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseDownloadPercent(%q) = %v, %v; want %v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestParseKeyValueInt(t *testing.T) {
	if n, ok := parseKeyValueInt("out_time_us=123456", "out_time_us"); !ok || n != 123456 {
		t.Errorf("plain = %d, %v", n, ok)
	}
	if n, ok := parseKeyValueInt("out_time_us=  123 ", "out_time_us"); !ok || n != 123 {
		t.Errorf("padded = %d, %v", n, ok)
	}
	if _, ok := parseKeyValueInt("out_time_ms=123", "out_time_us"); ok {
		t.Error("sibling key matched")
	}
	if _, ok := parseKeyValueInt("out_time_us=abc", "out_time_us"); ok {
		t.Error("non-integer value matched")
	}
	if _, ok := parseKeyValueInt("progress=end", "out_time_us"); ok {
		t.Error("unrelated line matched")
	}
}

func TestFitsDimensions(t *testing.T) {
	cases := []struct {
		w, h int
		want bool
	}{
		{1280, 720, true},
		{720, 1280, true},
		{640, 360, true},
		{1920, 1080, false},
		{1080, 1920, false},
		{3840, 2160, false},
		{721, 1280, false},
		{1281, 720, false},
		{0, 720, false},
		{720, 0, false},
	}
	for _, c := range cases {
		if got := fitsDimensions(c.w, c.h); got != c.want {
			t.Errorf("fitsDimensions(%d, %d) = %v; want %v", c.w, c.h, got, c.want)
		}
	}
}

func TestScaleFilterOrientation(t *testing.T) {
	landscape := "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
	portrait := "scale=w='min(720,iw)':h='min(1280,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
	if got := scaleFilter(1920, 1080); got != landscape {
		t.Errorf("landscape filter = %q", got)
	}
	if got := scaleFilter(1080, 1920); got != portrait {
		t.Errorf("portrait filter = %q", got)
	}
	if got := scaleFilter(720, 720); got != landscape {
		t.Errorf("square filter = %q", got)
	}
}

func TestAudioIsAAC(t *testing.T) {
	cases := []struct {
		name, payload string
		want          bool
	}{
		{"no audio", `{"streams":[]}`, true},
		{"aac", `{"streams":[{"codec_type":"audio","codec_name":"aac"}]}`, true},
		{"two aac", `{"streams":[{"codec_type":"audio","codec_name":"aac"},{"codec_type":"audio","codec_name":"aac"}]}`, true},
		{"opus", probeOpusAudioOnly, false},
		{"mixed", `{"streams":[{"codec_type":"audio","codec_name":"aac"},{"codec_type":"audio","codec_name":"opus"}]}`, false},
	}
	for _, c := range cases {
		var info probeInfo
		if err := jsonUnmarshalString(c.payload, &info); err != nil {
			t.Fatal(err)
		}
		if got := audioIsAAC(info); got != c.want {
			t.Errorf("%s: audioIsAAC = %v; want %v", c.name, got, c.want)
		}
	}
}

func TestAnalyzeVideoCompatFlags(t *testing.T) {
	var good probeInfo
	if err := jsonUnmarshalString(probeH264MuxedAAC, &good); err != nil {
		t.Fatal(err)
	}
	a := analyzeVideo(good)
	if !a.HasVideo || !a.HasAudio || !a.VideoOK || !a.AudioOK || a.DurationSec != 60.5 {
		t.Errorf("compatible source flagged incompatible: %+v", a)
	}
	if a.Width != 1280 || a.Height != 720 {
		t.Errorf("dimensions = %d×%d", a.Width, a.Height)
	}

	var vp9 probeInfo
	if err := jsonUnmarshalString(probeVP9Portrait, &vp9); err != nil {
		t.Fatal(err)
	}
	// No audio streams: AudioOK stays vacuously true and HasAudio gates it.
	if a := analyzeVideo(vp9); a.VideoOK || !a.AudioOK || a.HasAudio || a.Width != 1080 || a.Height != 1920 {
		t.Errorf("vp9 portrait analysis = %+v", a)
	}

	// A bad stream among good ones poisons the whole verdict.
	mixed := `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720},{"codec_type":"audio","codec_name":"aac"},{"codec_type":"audio","codec_name":"opus"}],"format":{"duration":"60.5"}}`
	var m probeInfo
	if err := jsonUnmarshalString(mixed, &m); err != nil {
		t.Fatal(err)
	}
	if a := analyzeVideo(m); a.VideoOK != true || a.AudioOK {
		t.Errorf("mixed stream analysis = %+v", a)
	}

	// Garbage duration falls back to zero (indeterminate progress).
	broken := `{"streams":[{"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":1280,"height":720}],"format":{"duration":"n/a"}}`
	var b probeInfo
	if err := jsonUnmarshalString(broken, &b); err != nil {
		t.Fatal(err)
	}
	if a := analyzeVideo(b); a.DurationSec != 0 || !a.VideoOK {
		t.Errorf("garbage duration analysis = %+v", a)
	}
}
