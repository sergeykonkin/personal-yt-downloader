package app

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runner executes a media command inside a job's working directory, feeding
// every stdout and stderr line to onLine as it is produced. Output is never
// accumulated in full: progress lines stream through the callback and only a
// small tail is retained for diagnostics.
type Runner interface {
	Run(ctx context.Context, dir, program string, onLine func(string), args ...string) error
}

// ProcessRunner kills the entire subprocess group on cancellation, including
// ffmpeg children, and bounds the diagnostic tail it keeps in memory.
type ProcessRunner struct {
	TailLines int
}

// tailBuffer retains the last lines of a command's combined output.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (t *tailBuffer) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) >= t.max {
		copy(t.lines, t.lines[1:])
		t.lines[len(t.lines)-1] = line
	} else {
		t.lines = append(t.lines, line)
	}
}

func (t *tailBuffer) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

func (r ProcessRunner) tailLimit() int {
	if r.TailLines > 0 {
		return r.TailLines
	}
	return 32
}

func (r ProcessRunner) Run(parent context.Context, dir, program string, onLine func(string), args ...string) error {
	started := time.Now()
	id := filepath.Base(filepath.Dir(dir))
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Serialize callbacks across the two pipe readers.
	var cb sync.Mutex
	emit := func(line string) {
		if onLine == nil {
			return
		}
		cb.Lock()
		onLine(line)
		cb.Unlock()
	}
	tail := &tailBuffer{max: r.tailLimit()}
	pump := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			tail.add(line)
			emit(line)
		}
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); pump(stdout) }()
	go func() { defer readers.Done(); pump(stderr) }()
	readers.Wait()

	waitErr := cmd.Wait()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	slog.Info("Media command completed", "id", id, "program", program, "exit_code", code, "duration_ms", time.Since(started).Milliseconds(), "canceled", ctx.Err() != nil)
	if parent.Err() != nil {
		return parent.Err()
	}
	if waitErr != nil {
		for _, line := range tail.snapshot() {
			if msg := strings.TrimSpace(line); msg != "" {
				slog.Info("Media command output", "id", id, "program", program, "tail", msg)
			}
		}
		return waitErr
	}
	return nil
}
