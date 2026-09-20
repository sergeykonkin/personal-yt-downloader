package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProcessRunnerStreamsBothStreams(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []string
	err := ProcessRunner{}.Run(context.Background(), dir, "sh",
		func(line string) { mu.Lock(); got = append(got, line); mu.Unlock() },
		"-c", "echo out1; echo err1 >&2; echo out2; pwd")
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, want := range []string{"out1", "err1", "out2", dir} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("missing line %q in %q", want, joined)
		}
	}
	if len(got) != 4 {
		t.Errorf("got %d lines; want 4: %q", len(got), got)
	}
}

func TestProcessRunnerPropagatesExitError(t *testing.T) {
	var got []string
	err := ProcessRunner{}.Run(context.Background(), t.TempDir(), "sh",
		func(line string) { got = append(got, line) },
		"-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("exit 3 reported as success")
	}
	if len(got) != 1 || got[0] != "boom" {
		t.Errorf("stderr lines = %q", got)
	}
}

// The child must die together with the group: if only the parent shell were
// killed, the orphaned sleep would hold the stdout pipe open and Run would
// only return after the 3s WaitDelay. A group SIGKILL returns in milliseconds.
func TestProcessRunnerKillsProcessGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 8)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- ProcessRunner{}.Run(ctx, t.TempDir(), "sh",
			func(line string) { lines <- line },
			"-c", "sleep 30 & echo started; wait")
	}()
	// Synchronize on the child being spawned before cancelling.
	select {
	case line := <-lines:
		if line != "started" {
			t.Fatalf("unexpected line %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("script never started")
	}
	start = time.Now()
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancellation took %v; process group not killed (WaitDelay fallback)", elapsed)
	}
	if err != context.Canceled {
		t.Errorf("cancelled run = %v; want context.Canceled", err)
	}
}

func TestProcessRunnerDeliversLongLines(t *testing.T) {
	var got []string
	err := ProcessRunner{}.Run(context.Background(), t.TempDir(), "sh",
		func(line string) { got = append(got, line) },
		"-c", "awk 'BEGIN{for(i=0;i<200000;i++) printf \"y\"; print \"\"}'")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 200000 || strings.Trim(got[0], "y") != "" {
		t.Errorf("long line mangled: %d lines, first len %d", len(got), len(got[0]))
	}
}

func TestTailBufferKeepsLastLines(t *testing.T) {
	tb := &tailBuffer{max: 3}
	for _, s := range []string{"a", "b", "c", "d"} {
		tb.add(s)
	}
	if got := tb.snapshot(); len(got) != 3 || got[0] != "b" || got[2] != "d" {
		t.Errorf("tail = %q", got)
	}
	tb2 := &tailBuffer{max: 3}
	for _, s := range []string{"a", "b"} {
		tb2.add(s)
	}
	if got := tb2.snapshot(); len(got) != 2 || got[0] != "a" {
		t.Errorf("short tail = %q", got)
	}
}
