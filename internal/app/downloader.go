package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DownloadError carries a message safe to show to the user.
type DownloadError string

func (e DownloadError) Error() string { return string(e) }

// publicError maps any pipeline failure to a user-safe message; internal
// command output never leaks.
func publicError(err error) string {
	switch {
	case errors.Is(err, ErrStorage):
		return ErrStorage.Error()
	case errors.Is(err, context.Canceled):
		return "The download was interrupted. Tap Retry to try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The download timed out. Tap Retry to try again."
	}
	var safe DownloadError
	if errors.As(err, &safe) {
		return string(safe)
	}
	return "Unable to download this video. It may be unavailable, private, or age-restricted."
}

// DownloadResult is what a finished pipeline hands back to the manager.
type DownloadResult struct {
	Title       string
	SizeBytes   int64
	DurationSec float64
}

// Reporter receives pipeline progress: stage plus a percentage when the total
// is measurable, or an indeterminate signal when it is not.
type Reporter func(stage string, progress float64, known bool)

// Downloader processes one job to a published MP4 or an error.
type Downloader interface {
	Download(ctx context.Context, j Job, report Reporter) (DownloadResult, error)
}

// VideoDownloader produces a fast-start MP4 (H.264/AAC, yuv420p, at most
// 1280×720 in either orientation), remuxing compatible sources without
// re-encoding. There are no duration, size, or job-timeout limits; network
// retries stay bounded inside yt-dlp.
type VideoDownloader struct {
	Runner Runner
	Root   string // data root; videos live under <root>/videos/<id>
	Fetch  MetadataFetcher
}

const (
	// yt-dlp's format grammar has no `&` inside a single bracket: conditions
	// are ANDed by stacking brackets instead, e.g. bv[height<=720][vcodec^=avc1].
	// Landscape sources are capped at 720 tall; portrait ones at 720 wide and
	// 1280 tall (so a 720x1280 portrait video needs no transcode). The later
	// fallbacks trade the avc1/mp4a preference for availability.
	videoSelector = "bv[height<=720][vcodec^=avc1]" +
		"/bv[width<=720][height<=1280][vcodec^=avc1]" +
		"/bv[height<=720]" +
		"/bv[width<=720][height<=1280]" +
		"/b[height<=720][vcodec^=avc1][acodec^=mp4a]" +
		"/b[width<=720][height<=1280][vcodec^=avc1][acodec^=mp4a]" +
		"/b[height<=720]" +
		"/b[width<=720][height<=1280]" +
		"/bv[vcodec^=avc1]/bv/b[vcodec^=avc1]/b"
	audioSelector = "ba[acodec^=mp4a]/ba"
)

func (d VideoDownloader) videoDir(id string) string { return filepath.Join(d.Root, "videos", id) }
func (d VideoDownloader) workDir(id string) string  { return filepath.Join(d.videoDir(id), "work") }
func (d VideoDownloader) videoFile(id string) string {
	return filepath.Join(d.videoDir(id), "video.mp4")
}

func (d VideoDownloader) Download(ctx context.Context, j Job, report Reporter) (DownloadResult, error) {
	meta := Metadata{ID: j.ID, Title: j.Title, DurationSec: j.DurationSec}
	if meta.Title == "" {
		fetched, err := d.Fetch.Fetch(ctx, j.ID)
		if err != nil {
			return DownloadResult{}, err
		}
		meta = fetched
	}
	// From here on, the title is known: carry it back even when a later
	// stage fails, so the card keeps its title through errors.
	fail := func(err error) (DownloadResult, error) {
		return DownloadResult{Title: meta.Title, DurationSec: meta.DurationSec}, err
	}
	work := d.workDir(j.ID)
	if err := os.RemoveAll(work); err != nil {
		return fail(ErrStorage)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return fail(ErrStorage)
	}
	defer os.RemoveAll(work)

	report(StageDownloadVideo, 0, false)
	videoPath, err := d.runYtDLP(ctx, work, j.ID, "video", videoSelector, StageDownloadVideo, report)
	if err != nil {
		return fail(err)
	}
	videoProbe, err := d.probe(ctx, work, videoPath)
	if err != nil {
		return fail(err)
	}
	source := analyzeVideo(videoProbe)
	if !source.HasVideo {
		return fail(DownloadError("The download does not contain a video stream."))
	}

	audioPath := ""
	audioOK := source.AudioOK
	if !source.HasAudio {
		report(StageDownloadAudio, 0, false)
		audioPath, err = d.runYtDLP(ctx, work, j.ID, "audio", audioSelector, StageDownloadAudio, report)
		if err != nil {
			return fail(err)
		}
		audioProbe, err := d.probe(ctx, work, audioPath)
		if err != nil {
			return fail(err)
		}
		audioOK = audioIsAAC(audioProbe)
	}

	report(StageProcessing, 0, source.DurationSec > 0)
	if err := d.runFFmpeg(ctx, work, videoPath, audioPath, source, audioOK, report); err != nil {
		return fail(err)
	}

	report(StageVerifying, 0, false)
	output := filepath.Join(work, "output.mp4")
	finalProbe, err := d.probe(ctx, work, output)
	if err != nil {
		return fail(err)
	}
	final := analyzeVideo(finalProbe)
	finalAudioOK := audioIsAAC(finalProbe)
	if !final.HasVideo || !final.VideoOK || !finalAudioOK {
		return fail(DownloadError("Unable to produce a compatible MP4."))
	}
	if source.DurationSec > 0 && final.DurationSec > 0 && final.DurationSec < source.DurationSec-2 {
		return fail(DownloadError("The prepared file is incomplete."))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	size, err := publish(work, output, d.videoFile(j.ID))
	if err != nil {
		return fail(ErrStorage)
	}
	slog.Info("File published", "id", j.ID, "bytes", size)
	return DownloadResult{Title: meta.Title, SizeBytes: size, DurationSec: meta.DurationSec}, nil
}

// runYtDLP downloads one stream (video- or audio-only) and reports its
// percentage as lines arrive.
func (d VideoDownloader) runYtDLP(ctx context.Context, dir, id, prefix, selector, stage string, report Reporter) (string, error) {
	args := append(ytDLPBase(),
		"--format", selector,
		"--match-filters", "!is_live & !is_upcoming",
		"--abort-on-unavailable-fragments",
		"--output", prefix+".%(ext)s",
		"--print", "after_move:filepath",
		// --print implies quiet mode; ask for progress explicitly and pin its
		// format to what parseDownloadPercent expects. The delta rate-limits
		// the lines; the UI polls every 2s anyway.
		"--progress",
		"--progress-template", "download:[download] %(progress._percent_str)s",
		"--progress-delta", "1",
		"--", "https://www.youtube.com/watch?v="+id)
	var printed []string
	err := d.Runner.Run(ctx, dir, "yt-dlp", func(line string) {
		if pct, ok := parseDownloadPercent(line); ok {
			report(stage, pct, true)
			return
		}
		// --print output has no [tag]; keep the last few such lines.
		if line != "" && line[0] != '[' {
			printed = append(printed, line)
			if len(printed) > 4 {
				printed = printed[len(printed)-4:]
			}
		}
	}, args...)
	if err != nil {
		return "", err
	}
	if len(printed) == 0 {
		return "", DownloadError("No complete stream was downloaded.")
	}
	path := strings.TrimSpace(printed[len(printed)-1])
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), prefix+".") || strings.ContainsAny(path, "\r\n") {
		return "", DownloadError("No complete stream was downloaded.")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", DownloadError("No complete stream was downloaded.")
	}
	return path, nil
}

// runFFmpeg remuxes or transcodes the source(s) into output.mp4, reporting
// progress from out_time against the source duration.
func (d VideoDownloader) runFFmpeg(ctx context.Context, dir, videoPath, audioPath string, source probeAnalysis, audioOK bool, report Reporter) (err error) {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats",
		"-progress", "pipe:1", "-y", "-i", videoPath}
	if audioPath != "" {
		args = append(args, "-i", audioPath)
	}
	args = append(args, "-map", "0:v:0")
	if audioPath != "" {
		args = append(args, "-map", "1:a:0")
	} else {
		args = append(args, "-map", "0:a:0?")
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")
	switch {
	case source.VideoOK && audioOK:
		args = append(args, "-c", "copy")
	case source.VideoOK:
		args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "128k")
	default:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
			"-pix_fmt", "yuv420p", "-vf", scaleFilter(source.Width, source.Height))
		if audioOK {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "128k")
		}
	}
	args = append(args, "-movflags", "+faststart", filepath.Join(dir, "output.mp4"))

	known := source.DurationSec > 0
	report(StageProcessing, 0, known)
	err = d.Runner.Run(ctx, dir, "ffmpeg", func(line string) {
		if us, ok := parseKeyValueInt(line, "out_time_us"); ok && known {
			pct := float64(us) / (source.DurationSec * 1e6) * 100
			report(StageProcessing, pct, true)
		}
		if line == "progress=end" && known {
			report(StageProcessing, 100, true)
		}
	}, args...)
	return err
}

func (d VideoDownloader) probe(ctx context.Context, dir, path string) (probeInfo, error) {
	var lines []string
	err := d.Runner.Run(ctx, dir, "ffprobe",
		func(line string) { lines = append(lines, line) },
		"-v", "error", "-show_entries", "stream=codec_type,codec_name,pix_fmt,width,height:format=duration",
		"-of", "json", path)
	if err != nil {
		if ctx.Err() != nil {
			return probeInfo{}, ctx.Err()
		}
		return probeInfo{}, DownloadError("Unable to inspect the downloaded media.")
	}
	var info probeInfo
	if json.Unmarshal([]byte(strings.Join(lines, "\n")), &info) != nil {
		return probeInfo{}, DownloadError("Unable to inspect the downloaded media.")
	}
	return info, nil
}

type probeInfo struct {
	Streams []struct {
		Type        string `json:"codec_type"`
		Codec       string `json:"codec_name"`
		PixelFormat string `json:"pix_fmt"`
		Height      int    `json:"height"`
		Width       int    `json:"width"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type probeAnalysis struct {
	HasVideo, HasAudio bool
	VideoOK, AudioOK   bool
	Width, Height      int
	DurationSec        float64
}

// fitsDimensions enforces the ceiling in either orientation: 1280×720 or
// 720×1280, so portrait video keeps its quality.
func fitsDimensions(width, height int) bool {
	return width > 0 && height > 0 &&
		((width <= 1280 && height <= 720) || (width <= 720 && height <= 1280))
}

func analyzeVideo(info probeInfo) probeAnalysis {
	// VideoOK/AudioOK start true and are cleared by any incompatible stream;
	// callers consult HasVideo/HasAudio before trusting them.
	a := probeAnalysis{VideoOK: true, AudioOK: true}
	for _, stream := range info.Streams {
		if stream.Type == "video" {
			if !a.HasVideo {
				a.HasVideo, a.Width, a.Height = true, stream.Width, stream.Height
			}
			a.VideoOK = a.VideoOK && stream.Codec == "h264" && stream.PixelFormat == "yuv420p" && fitsDimensions(stream.Width, stream.Height)
		}
		if stream.Type == "audio" {
			a.HasAudio = true
			a.AudioOK = a.AudioOK && stream.Codec == "aac"
		}
	}
	if duration, err := strconv.ParseFloat(info.Format.Duration, 64); err == nil {
		a.DurationSec = duration
	}
	return a
}

func audioIsAAC(info probeInfo) bool {
	hasAudio := false
	ok := true
	for _, stream := range info.Streams {
		if stream.Type == "audio" {
			hasAudio = true
			ok = ok && stream.Codec == "aac"
		}
	}
	return !hasAudio || ok
}

// scaleFilter fits the source into 1280×720 (landscape) or 720×1280
// (portrait) while preserving aspect ratio.
func scaleFilter(width, height int) string {
	if width >= height {
		return "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
	}
	return "scale=w='min(720,iw)':h='min(1280,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
}

// publish moves the verified MP4 into place atomically: fsync the file,
// rename over the destination, fsync the directory.
func publish(dir, output, dest string) (int64, error) {
	f, err := os.Open(output)
	if err != nil {
		return 0, err
	}
	syncErr := f.Sync()
	info, statErr := f.Stat()
	closeErr := f.Close()
	if syncErr != nil {
		return 0, syncErr
	}
	if statErr != nil || closeErr != nil {
		return 0, statErr
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, errors.New("output is not a regular file")
	}
	if err := os.Rename(output, dest); err != nil {
		return 0, err
	}
	if d, err := os.Open(filepath.Dir(dest)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return info.Size(), nil
}

// parseDownloadPercent extracts the percentage from a yt-dlp progress line
// such as "[download]  42.7% of ~10.50MiB at 1 MiB/s ETA 00:08".
func parseDownloadPercent(line string) (float64, bool) {
	const prefix = "[download]"
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	rest := strings.TrimSpace(line[len(prefix):])
	i := strings.IndexByte(rest, '%')
	if i <= 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(rest[:i], 64)
	if err != nil || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

func parseKeyValueInt(line, key string) (int64, bool) {
	if !strings.HasPrefix(line, key+"=") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(line[len(key)+1:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
