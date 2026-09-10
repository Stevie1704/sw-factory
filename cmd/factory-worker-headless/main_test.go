package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReadBoundedRetainsTheTail verifies that a late machine failure event is
// still available after the helper bounds a large process stream.
func TestReadBoundedRetainsTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	data := strings.Repeat("head\n", 64) + `{"type":"turn.failed","error":{"message":"rate limit"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, truncated := readBounded(path, 256)
	if !truncated {
		t.Fatal("readBounded() truncated = false, want true")
	}
	if !strings.Contains(string(got), `"type":"turn.failed"`) {
		t.Fatalf("readBounded() = %q, want the tail event retained", got)
	}
}

// TestReadBoundedHonorsSmallLimits verifies the truncation marker never causes
// the helper to exceed the caller's requested byte bound.
func TestReadBoundedHonorsSmallLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 128)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, 1, len(outputTruncationMarker) - 1, len(outputTruncationMarker), len(outputTruncationMarker) + 1} {
		got, truncated := readBounded(path, limit)
		if !truncated || len(got) > limit {
			t.Fatalf("readBounded(limit=%d) = %d bytes, truncated=%t; want at most the limit", limit, len(got), truncated)
		}
	}
}

// TestBoundedFileWriterRetainsTheTail verifies that the live pipe drain keeps a
// late failure event for the final bounded inspection.
func TestBoundedFileWriterRetainsTheTail(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	writer := &boundedFileWriter{file: file, limit: 96}
	if _, err := writer.Write([]byte(strings.Repeat("head\n", 40) + `{"type":"turn.failed"}`)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.finalize(); err != nil {
		t.Fatalf("finalize() error = %v", err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > writer.limit {
		t.Fatalf("captured bytes = %d, want at most %d", len(data), writer.limit)
	}
	if !strings.Contains(string(data), `"type":"turn.failed"`) {
		t.Fatalf("captured output = %q, want the late failure event", data)
	}
}

// TestBoundedFileWriterHonorsSmallLimits verifies finalization preserves the
// writer's hard byte bound even when the marker is larger than the limit.
func TestBoundedFileWriterHonorsSmallLimits(t *testing.T) {
	for _, limit := range []int{0, 1, len(outputTruncationMarker) - 1, len(outputTruncationMarker), len(outputTruncationMarker) + 1} {
		directory := t.TempDir()
		file, err := os.CreateTemp(directory, "stdout-")
		if err != nil {
			t.Fatal(err)
		}
		writer := &boundedFileWriter{file: file, limit: limit}
		if _, err := writer.Write([]byte(strings.Repeat("x", 128))); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := writer.finalize(); err != nil {
			file.Close()
			t.Fatal(err)
		}
		data, err := os.ReadFile(file.Name())
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > limit {
			t.Fatalf("captured bytes at limit %d = %d, want at most the limit", limit, len(data))
		}
	}
}

// TestCancelPIDEscalatesWithoutReportingSuccessEarly verifies a process that
// ignores SIGTERM is killed before cancellation returns.
func TestCancelPIDEscalatesWithoutReportingSuccessEarly(t *testing.T) {
	oldGrace, oldPoll := headlessCancellationGrace, headlessCancellationPoll
	headlessCancellationGrace = 20 * time.Millisecond
	headlessCancellationPoll = 2 * time.Millisecond
	defer func() {
		headlessCancellationGrace, headlessCancellationPoll = oldGrace, oldPoll
	}()

	command := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do :; done")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	if err := cancelPID(command.Process.Pid); err != nil {
		t.Fatalf("cancelPID() error = %v", err)
	}
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("cancelPID() returned before the child was reaped")
	}
	if processAlive(command.Process.Pid) {
		t.Fatal("cancelPID() returned while the child was still alive")
	}
}
