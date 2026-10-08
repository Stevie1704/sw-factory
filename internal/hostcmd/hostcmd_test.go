package hostcmd_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/hostcmd"
)

// TestRunStopsACommandThatNeverEndsAtItsDeadline verifies that a hung host
// command returns a typed timeout error soon after its deadline and that the
// command and the child it started are both gone.
func TestRunStopsACommandThatNeverEndsAtItsDeadline(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "pids")
	// The shell starts a child sleep and records both process IDs, as gh and
	// git start helper processes of their own.
	script := `sleep 60 & echo "$$ $!" > "$1"; wait`
	started := time.Now()
	_, err := hostcmd.Run(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", script, "sh", pidFile},
		Timeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(started)

	var timeout *hostcmd.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Run() error = %v, want *hostcmd.TimeoutError", err)
	}
	if timeout.Operation != "sh -c" || timeout.Timeout != 200*time.Millisecond {
		t.Fatalf("timeout error = %#v, want operation %q and the 200ms deadline", timeout, "sh -c")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timeout error matches context.DeadlineExceeded; a supervisor would read it as its own shutdown")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run() returned after %s, want shortly after the 200ms deadline", elapsed)
	}
	for _, pid := range recordedPIDs(t, pidFile) {
		if processAlive(pid) {
			t.Fatalf("process %d is still running after the deadline", pid)
		}
	}
}

// TestRunRejectsOutputPastTheLimit verifies that a command that writes more
// than the output limit returns a typed output-limit error and no output.
func TestRunRejectsOutputPastTheLimit(t *testing.T) {
	t.Parallel()

	output, err := hostcmd.Run(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", "head -c " + strconv.Itoa(hostcmd.OutputLimit+1) + " /dev/zero"},
		Timeout: 30 * time.Second,
	})

	var limit *hostcmd.OutputLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Run() error = %v, want *hostcmd.OutputLimitError", err)
	}
	if limit.Stream != "stdout" || limit.Limit != hostcmd.OutputLimit {
		t.Fatalf("output-limit error = %#v, want stdout and the package limit", limit)
	}
	if len(output.Stdout) != 0 || len(output.Stderr) != 0 {
		t.Fatal("Run() returned partial output after an overflow")
	}
}

// TestRunReturnsOutputAndProcessFailure verifies the ordinary paths: stdout
// for a success, and stderr with the process error for a failure.
func TestRunReturnsOutputAndProcessFailure(t *testing.T) {
	t.Parallel()

	output, err := hostcmd.Run(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", `printf '%s' "$PROBE"`},
		Env:     []string{"PROBE=value"},
		Timeout: 30 * time.Second,
	})
	if err != nil || string(output.Stdout) != "value" {
		t.Fatalf("Run() = %q, %v, want the environment value on stdout", output.Stdout, err)
	}

	output, err = hostcmd.Run(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", "echo broken >&2; exit 3"},
		Timeout: 30 * time.Second,
	})
	if err == nil || strings.TrimSpace(string(output.Stderr)) != "broken" {
		t.Fatalf("Run() = stderr %q, error %v, want the failure and its stderr", output.Stderr, err)
	}
}

// TestRunReportsCallerCancellationAsTheContextError verifies that a caller
// that stops, such as a supervisor that shuts down, gets its own context error
// and not a retryable timeout.
func TestRunReportsCallerCancellationAsTheContextError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := hostcmd.Run(ctx, hostcmd.Command{
		Name:    "sleep",
		Args:    []string{"60"},
		Timeout: time.Minute,
	})
	var timeout *hostcmd.TimeoutError
	if errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want the caller's context.DeadlineExceeded", err)
	}
}

// TestStreamStopsACommandWhoseChildKeepsTheOutputOpen verifies that a
// streamed command gets the same deadline as Run: a child that keeps stdout
// open cannot hold the call past the deadline, and the output streamed before
// the deadline stays in the destination.
func TestStreamStopsACommandWhoseChildKeepsTheOutputOpen(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "pids")
	var destination strings.Builder
	started := time.Now()
	err := hostcmd.Stream(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", `printf partial; sleep 60 & echo "$$ $!" > "$1"; wait`, "sh", pidFile},
		Timeout: 200 * time.Millisecond,
	}, &destination)

	var timeout *hostcmd.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Stream() error = %v, want *hostcmd.TimeoutError", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Stream() returned after %s, want shortly after the 200ms deadline", elapsed)
	}
	if destination.String() != "partial" {
		t.Fatalf("streamed output = %q, want the bytes written before the deadline", destination.String())
	}
	for _, pid := range recordedPIDs(t, pidFile) {
		if processAlive(pid) {
			t.Fatalf("process %d is still running after the deadline", pid)
		}
	}
}

// TestStreamRemovesUnsetVariablesFromTheInheritedEnvironment verifies that a
// caller can drop an inherited variable instead of setting it to an empty
// value, which Git would read as a path.
func TestStreamRemovesUnsetVariablesFromTheInheritedEnvironment(t *testing.T) {
	t.Setenv("HOSTCMD_PROBE", "inherited")

	var destination strings.Builder
	err := hostcmd.Stream(context.Background(), hostcmd.Command{
		Name:    "sh",
		Args:    []string{"-c", `printf '%s' "${HOSTCMD_PROBE-unset}"`},
		Unset:   []string{"HOSTCMD_PROBE"},
		Timeout: 30 * time.Second,
	}, &destination)
	if err != nil || destination.String() != "unset" {
		t.Fatalf("Stream() = %q, %v, want the variable removed", destination.String(), err)
	}
}

// recordedPIDs waits for the script to record its process IDs and parses them.
func recordedPIDs(t *testing.T, path string) []int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded process IDs: %v", err)
	}
	var pids []int
	for _, field := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("parse process ID %q: %v", field, err)
		}
		pids = append(pids, pid)
	}
	if len(pids) != 2 {
		t.Fatalf("recorded process IDs = %v, want the shell and its child", pids)
	}
	return pids
}

// processAlive reports whether a process ID still names a running process. A
// zombie that waits for its parent to collect it counts as gone, because it
// can no longer run or hold a pipe.
func processAlive(pid int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
		if zombie(pid) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// zombie reports whether ps shows the process in the zombie state.
func zombie(pid int) bool {
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}
