package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVideoIDAccepts(t *testing.T) {
	const want = "dQw4w9WgXcQ"
	for _, raw := range []string{
		"https://www.youtube.com/watch?v=" + want,
		"https://www.youtube.com/watch?v=" + want + "&t=30s",
		"https://m.youtube.com/watch?v=" + want,
		"https://youtube.com/watch?v=" + want + "&feature=share",
		"https://music.youtube.com/watch?v=" + want,
		"https://www.youtube.com/shorts/" + want,
		"https://www.youtube.com/live/" + want,
		"https://www.youtube.com/embed/" + want,
		"https://youtu.be/" + want + "?t=42",
		"https://youtu.be/" + want,
		"http://www.youtube.com/watch?v=" + want,
	} {
		id, err := VideoID(raw)
		if err != nil || id != want {
			t.Errorf("VideoID(%q) = %q, %v; want %q, nil", raw, id, err, want)
		}
	}
}

func TestVideoIDRejects(t *testing.T) {
	for _, raw := range []string{
		"",
		"not a url",
		"https://example.com/watch?v=dQw4w9WgXcQ",
		"https://youtube.com/watch",
		"https://youtube.com/watch?v=",
		"https://youtube.com/watch?v=dQw4w9WgXcQ&v=other123456",
		"https://youtube.com/playlist?list=PL1234567890",
		"https://youtube.com/watch/extra/dQw4w9WgXcQ",
		"https://youtube.com/shorts",
		"https://youtube.com/shorts/dQw4w9WgXcQ/extra",
		"https://youtu.be/dQw4w9WgXcQ/extra",
		"https://user@youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtube.com:8443/watch?v=dQw4w9WgXcQ",
		"ftp://youtube.com/watch?v=dQw4w9WgXcQ",
		"https://dQw4w9WgXcQ@youtu.be/x",
	} {
		if id, err := VideoID(raw); err == nil {
			t.Errorf("VideoID(%q) = %q; want error", raw, id)
		}
	}
}

func TestVideoIDLengthBound(t *testing.T) {
	long := "https://youtu.be/dQw4w9WgXcQ?" + repeatChar('x', 5000)
	if _, err := VideoID(long); err == nil {
		t.Error("overlong URL accepted")
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func TestSafeFilename(t *testing.T) {
	cases := []struct {
		title, id, want string
	}{
		{"Simple Title", "abcdefghijk", "Simple Title.mp4"},
		{"", "abcdefghijk", "abcdefghijk.mp4"},
		{"  ", "abcdefghijk", "abcdefghijk.mp4"},
		{`a/b\c:d*e?f"g<h>i|j`, "abcdefghijk", "a_b_c_d_e_f_g_h_i_j.mp4"},
		{"ends with dots...", "abcdefghijk", "ends with dots.mp4"},
		{"café 日本語", "abcdefghijk", "café 日本語.mp4"},
	}
	for _, c := range cases {
		if got := SafeFilename(c.title, c.id); got != c.want {
			t.Errorf("SafeFilename(%q) = %q; want %q", c.title, got, c.want)
		}
	}
}

func TestSafeFilenameControlsAndBoundedLength(t *testing.T) {
	title := "x\x00\x7fy‍z"
	if got := SafeFilename(title, "abcdefghijk"); got != "x__y_z.mp4" {
		t.Errorf("control runes not replaced: %q", got)
	}
	long := make([]rune, 500)
	for i := range long {
		long[i] = 'ö'
	}
	got := SafeFilename(string(long), "abcdefghijk")
	if runeCount := len([]rune(got)); runeCount > 180+4 || runeCount < 4 {
		t.Errorf("unbounded filename: %d runes", runeCount)
	}
}

func TestJobViewJSON(t *testing.T) {
	t.Run("default title and omitted progress", func(t *testing.T) {
		v := view(Job{ID: "abcdefghijk", Status: StatusQueued, CreatedAt: time.Unix(0, 0).UTC()}, StageQueued, 0, false)
		if v.Title != "Fetching title…" {
			t.Errorf("title = %q", v.Title)
		}
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if _, present := m["progress"]; present {
			t.Error("unknown progress must be omitted")
		}
		if _, present := m["error"]; present {
			t.Error("error must be omitted for non-failed jobs")
		}
	})
	t.Run("progress clamped and included", func(t *testing.T) {
		v := view(Job{ID: "abcdefghijk", Status: StatusDownloading, CreatedAt: time.Unix(0, 0).UTC()}, StageDownloadVideo, 150, true)
		if v.Progress == nil || *v.Progress != 100 {
			t.Errorf("progress = %v; want 100", v.Progress)
		}
	})
	t.Run("failed job carries its error", func(t *testing.T) {
		v := view(Job{ID: "abcdefghijk", Status: StatusFailed, Error: "boom", CreatedAt: time.Unix(0, 0).UTC()}, StageFailed, 0, false)
		if v.Error != "boom" {
			t.Errorf("error = %q", v.Error)
		}
	})
}

func TestStatusForStage(t *testing.T) {
	cases := map[string]string{
		StageDownloadVideo: StatusDownloading,
		StageDownloadAudio: StatusDownloading,
		StageProcessing:   StatusProcessing,
		StageVerifying:     StatusVerifying,
		StageReady:         StatusReady,
		StageFailed:        StatusFailed,
		StageQueued:        StatusQueued,
		"anything-else":    StatusQueued,
	}
	for stage, want := range cases {
		if got := statusForStage(stage); got != want {
			t.Errorf("statusForStage(%q) = %q; want %q", stage, got, want)
		}
	}
}

func TestJobUnfinished(t *testing.T) {
	for status, want := range map[string]bool{
		StatusQueued: true, StatusDownloading: true, StatusProcessing: true, StatusVerifying: true,
		StatusReady: false, StatusFailed: false,
	} {
		if got := (Job{Status: status}).unfinished(); got != want {
			t.Errorf("status %q unfinished() = %v; want %v", status, got, want)
		}
	}
}
