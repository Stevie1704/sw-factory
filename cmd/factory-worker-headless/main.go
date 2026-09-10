// Command factory-worker-headless owns the small durable process protocol used
// by terminal-free harnesses inside a factory worker.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	stateStatus       = "status"
	statePID          = "pid"
	stateExitCode     = "exit_code"
	stateStdout       = "stdout"
	stateStderr       = "stderr"
	stateStdoutLimit  = "stdout_truncated"
	stateStderrLimit  = "stderr_truncated"
	stateCancelMarker = "cancelled"
	stateLock         = "lock"
	inspectionBytes   = 512 << 10
)

const outputTruncationMarker = "\n[...output truncated...]\n"

var (
	// headlessCancellationGrace is the time allowed for a detached process to
	// handle SIGTERM before the helper escalates to SIGKILL.
	headlessCancellationGrace = 5 * time.Second
	// headlessCancellationPoll is the interval between process liveness checks
	// during graceful termination and forced termination.
	headlessCancellationPoll = 50 * time.Millisecond
)

// processStatus is the on-disk spelling shared with worker.HeadlessStatus.
type processStatus string

const (
	statusStarting  processStatus = "starting"
	statusRunning   processStatus = "running"
	statusExited    processStatus = "exited"
	statusCancelled processStatus = "cancelled"
	statusLost      processStatus = "lost"
	statusMissing   processStatus = "missing"
)

// inspectionWire is the bounded JSON response consumed by the worker adapter.
type inspectionWire struct {
	// Status is the helper-owned process lifecycle state.
	Status string `json:"status"`
	// ExitCode is the child process exit code after termination.
	ExitCode int `json:"exit_code"`
	// Stdout is bounded machine-readable child output encoded as base64.
	Stdout string `json:"stdout"`
	// Stderr is bounded child diagnostic output encoded as base64.
	Stderr string `json:"stderr"`
	// StdoutTruncated reports that stdout exceeded the retained bound.
	StdoutTruncated bool `json:"stdout_truncated"`
	// StderrTruncated reports that stderr exceeded the retained bound.
	StderrTruncated bool `json:"stderr_truncated"`
}

// main dispatches the fixed helper operations and keeps errors on stderr so
// inspect can reserve stdout for one machine-readable JSON document.
func main() {
	if len(os.Args) < 2 {
		fail(errors.New("factory-worker-headless requires run, inspect, or cancel"))
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runProcess(os.Args[2:])
	case "inspect":
		err = inspectProcess(os.Args[2:])
	case "cancel":
		err = cancelProcess(os.Args[2:])
	default:
		err = fmt.Errorf("unknown factory-worker-headless operation %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

// fail prints a bounded control-plane error and exits nonzero.
func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// runProcess starts one command and records its lifecycle and bounded output.
func runProcess(arguments []string) error {
	flags := flag.NewFlagSet("factory-worker-headless run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", "", "durable process state directory")
	replace := flags.Bool("replace", false, "replace a prior lost or exited process")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	command := append([]string(nil), flags.Args()...)
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if err := validateStateDir(*stateDir); err != nil {
		return err
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return errors.New("headless process command is required")
	}
	for _, argument := range command {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("headless process command contains a NUL byte")
		}
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return fmt.Errorf("create headless state: %w", err)
	}
	releaseStateLock, err := acquireStateLock(*stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = releaseStateLock() }()
	status, err := readStatus(*stateDir)
	if err != nil {
		return err
	}
	if !*replace {
		if status != statusMissing && status != statusLost {
			return fmt.Errorf("headless process already has state %q", status)
		}
	} else if status == statusStarting || status == statusRunning || status == statusCancelled {
		pid, pidErr := readPID(*stateDir)
		if pidErr == nil && processAlive(pid) {
			return errors.New("cannot replace a headless process that is still running")
		}
	}
	if err := clearStateForReplacement(*stateDir); err != nil {
		return err
	}
	if err := writeState(*stateDir, stateStatus, string(statusStarting)); err != nil {
		return err
	}
	if err := os.WriteFile(statePath(*stateDir, stateStdout), nil, 0o600); err != nil {
		return fmt.Errorf("create headless stdout: %w", err)
	}
	if err := os.WriteFile(statePath(*stateDir, stateStderr), nil, 0o600); err != nil {
		return fmt.Errorf("create headless stderr: %w", err)
	}
	_ = os.Remove(statePath(*stateDir, stateCancelMarker))

	stdout, err := os.OpenFile(statePath(*stateDir, stateStdout), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open headless stdout: %w", err)
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.OpenFile(statePath(*stateDir, stateStderr), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open headless stderr: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	commandProcess := exec.Command(command[0], command[1:]...)
	stdoutWriter := &boundedFileWriter{file: stdout, limit: worker.MaxCapturedOutputBytes}
	stderrWriter := &boundedFileWriter{file: stderr, limit: worker.MaxCapturedOutputBytes}
	commandProcess.Stdout = stdoutWriter
	commandProcess.Stderr = stderrWriter
	commandProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := commandProcess.Start(); err != nil {
		_, _ = stderr.WriteString("process start failed\n")
		_ = writeState(*stateDir, stateStatus, string(statusExited))
		_ = writeState(*stateDir, stateExitCode, "127")
		return fmt.Errorf("start headless command: %w", err)
	}
	if err := writeState(*stateDir, statePID, strconv.Itoa(commandProcess.Process.Pid)); err != nil {
		_ = terminate(commandProcess.Process.Pid)
		return err
	}
	if err := writeState(*stateDir, stateStatus, string(statusRunning)); err != nil {
		_ = terminate(commandProcess.Process.Pid)
		return err
	}
	// A cancellation can arrive while the helper is still publishing its
	// starting markers. Recheck after the PID exists so that race becomes a
	// real process cancellation instead of a stale running state.
	if readBoolean(statePath(*stateDir, stateCancelMarker)) {
		_ = terminate(commandProcess.Process.Pid)
	}
	if err := releaseStateLock(); err != nil {
		_ = killProcess(commandProcess.Process.Pid)
		return err
	}

	wait := make(chan error, 1)
	go func() { wait <- commandProcess.Wait() }()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-interrupt:
		_ = writeState(*stateDir, stateCancelMarker, "1")
		_ = terminate(commandProcess.Process.Pid)
		waitErr = waitForCommandExit(wait, commandProcess.Process.Pid)
	}

	_ = stdoutWriter.finalize()
	_ = stderrWriter.finalize()
	if _, err := os.Stat(statePath(*stateDir, stateCancelMarker)); err == nil {
		_ = writeState(*stateDir, stateStatus, string(statusCancelled))
	} else {
		_ = writeState(*stateDir, stateStatus, string(statusExited))
	}
	_ = writeState(*stateDir, stateExitCode, strconv.Itoa(exitCode(waitErr)))
	if stdoutWriter.truncated {
		_ = writeState(*stateDir, stateStdoutLimit, "1")
	}
	if stderrWriter.truncated {
		_ = writeState(*stateDir, stateStderrLimit, "1")
	}
	return nil
}

// inspectProcess prints one bounded state response and marks a vanished
// running process as lost for coordinator recovery.
func inspectProcess(arguments []string) error {
	stateDir, err := parseStateDir(arguments)
	if err != nil {
		return err
	}
	status, err := readStatus(stateDir)
	if err != nil {
		return err
	}
	if status == statusStarting || status == statusRunning || status == statusCancelled {
		pid, pidErr := readPID(stateDir)
		if pidErr == nil && processAlive(pid) {
			status = statusRunning
			if readStatusValue, _ := readStatus(stateDir); readStatusValue == statusCancelled {
				_ = writeState(stateDir, stateStatus, string(statusRunning))
			}
		} else if pidErr == nil && status != statusCancelled {
			status = statusLost
			_ = writeState(stateDir, stateStatus, string(statusLost))
		}
	}
	stdout, stdoutTruncated := readBounded(statePath(stateDir, stateStdout), inspectionBytes)
	stderr, stderrTruncated := readBounded(statePath(stateDir, stateStderr), inspectionBytes)
	if readBoolean(statePath(stateDir, stateStdoutLimit)) {
		stdoutTruncated = true
	}
	if readBoolean(statePath(stateDir, stateStderrLimit)) {
		stderrTruncated = true
	}
	wire := inspectionWire{
		Status: string(status), ExitCode: readExitCode(stateDir),
		Stdout: base64.StdEncoding.EncodeToString(stdout), Stderr: base64.StdEncoding.EncodeToString(stderr),
		StdoutTruncated: stdoutTruncated, StderrTruncated: stderrTruncated,
	}
	return json.NewEncoder(os.Stdout).Encode(wire)
}

// cancelProcess signals a running detached command and returns only after the
// child has stopped. It escalates from SIGTERM to SIGKILL and leaves the state
// running when the process cannot be proven dead.
func cancelProcess(arguments []string) error {
	stateDir, err := parseStateDir(arguments)
	if err != nil {
		return err
	}
	if _, err := os.Stat(stateDir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return fmt.Errorf("create headless state for cancellation: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect headless state directory: %w", err)
	}
	releaseStateLock, err := acquireStateLock(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = releaseStateLock() }()
	status, err := readStatus(stateDir)
	if err != nil {
		return err
	}
	if status != statusStarting && status != statusRunning {
		if status == statusMissing {
			// Reserve cancellation before a detached helper creates its state.
			// A concurrent launcher waits on the same lock and will reject the
			// cancelled reservation instead of starting after this command returns.
			return writeState(stateDir, stateStatus, string(statusCancelled))
		}
		return nil
	}
	pid, err := readPID(stateDir)
	if err != nil {
		_ = writeState(stateDir, stateStatus, string(statusCancelled))
		return nil
	}
	_ = writeState(stateDir, stateCancelMarker, "1")
	if err := releaseStateLock(); err != nil {
		return err
	}
	if err := cancelPID(pid); err != nil {
		return err
	}
	return writeState(stateDir, stateStatus, string(statusCancelled))
}

// parseStateDir parses the one fixed helper path argument.
func parseStateDir(arguments []string) (string, error) {
	flags := flag.NewFlagSet("factory-worker-headless", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", "", "durable process state directory")
	if err := flags.Parse(arguments); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected headless helper argument")
	}
	if err := validateStateDir(*stateDir); err != nil {
		return "", err
	}
	return filepath.Clean(*stateDir), nil
}

// validateStateDir keeps the helper inside the role-home process namespace.
func validateStateDir(path string) error {
	clean := filepath.Clean(path)
	root := filepath.Clean("/home/factory/.factory-headless")
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || clean == root || !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return errors.New("headless state directory must be inside the role process namespace")
	}
	if strings.Contains(filepath.Base(clean), string(filepath.Separator)) || filepath.Base(clean) == "." || filepath.Base(clean) == ".." {
		return errors.New("headless state directory has an unsafe invocation identity")
	}
	return nil
}

// clearStateForReplacement removes only files owned by one invocation state.
func clearStateForReplacement(stateDir string) error {
	for _, name := range []string{statePID, stateExitCode, stateStdout, stateStderr, stateStdoutLimit, stateStderrLimit, stateCancelMarker} {
		if err := os.Remove(statePath(stateDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear headless state: %w", err)
		}
	}
	return nil
}

// acquireStateLock serializes the launch reservation and cancellation marker
// update across detached helper processes. The kernel releases the advisory
// lock if a helper is killed, so a coordinator restart cannot inherit a stale
// lock directory.
func acquireStateLock(stateDir string) (func() error, error) {
	file, err := os.OpenFile(statePath(stateDir, stateLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open headless state lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock headless state: %w", err)
	}
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		closeErr := file.Close()
		return errors.Join(unlockErr, closeErr)
	}, nil
}

// readStatus reads the durable state marker, treating an absent state as
// missing so fresh launch remains idempotent.
func readStatus(stateDir string) (processStatus, error) {
	data, err := os.ReadFile(statePath(stateDir, stateStatus))
	if errors.Is(err, os.ErrNotExist) {
		return statusMissing, nil
	}
	if err != nil {
		return "", fmt.Errorf("read headless status: %w", err)
	}
	status := processStatus(strings.TrimSpace(string(data)))
	switch status {
	case statusStarting, statusRunning, statusExited, statusCancelled, statusLost:
		return status, nil
	default:
		return "", fmt.Errorf("unknown headless status %q", status)
	}
}

// readPID reads the helper's child process identifier.
func readPID(stateDir string) (int, error) {
	data, err := os.ReadFile(statePath(stateDir, statePID))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errors.New("headless process pid is invalid")
	}
	return pid, nil
}

// readExitCode reads a terminal process code while defaulting incomplete
// startup state to zero.
func readExitCode(stateDir string) int {
	data, err := os.ReadFile(statePath(stateDir, stateExitCode))
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return value
}

// readBoolean reads a marker file without making its contents part of output.
func readBoolean(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readBounded returns no more than limit bytes and records truncation.
func readBounded(path string, limit int) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if len(data) <= limit {
		return data, false
	}
	if limit <= 0 {
		return nil, true
	}
	marker := []byte(outputTruncationMarker)
	if limit <= len(marker) {
		return append([]byte(nil), marker[:limit]...), true
	}
	retained := limit - len(marker)
	headLimit := retained / 2
	tailLimit := retained - headLimit
	result := make([]byte, 0, headLimit+len(marker)+tailLimit)
	result = append(result, data[:headLimit]...)
	result = append(result, marker...)
	if tailLimit > 0 {
		result = append(result, data[len(data)-tailLimit:]...)
	}
	return result, true
}

// writeState atomically replaces one small state marker.
func writeState(stateDir, name, value string) error {
	temporary, err := os.CreateTemp(stateDir, ".factory-headless-")
	if err != nil {
		return fmt.Errorf("create headless state marker: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect headless state marker: %w", err)
	}
	if _, err := temporary.WriteString(value + "\n"); err != nil {
		return fmt.Errorf("write headless state marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close headless state marker: %w", err)
	}
	if err := os.Rename(temporaryName, statePath(stateDir, name)); err != nil {
		return fmt.Errorf("publish headless state marker: %w", err)
	}
	return nil
}

// statePath joins only helper-owned marker names to a validated state root.
func statePath(stateDir, name string) string {
	return filepath.Join(stateDir, name)
}

// processAlive reports whether a child PID still exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// cancelPID stops one detached command without claiming success while its
// process is still observable. It first gives the process group a graceful
// window, then escalates to SIGKILL and reports an unresolved process to the
// caller rather than publishing a false terminal state.
func cancelPID(pid int) error {
	if !processAlive(pid) {
		return nil
	}
	_ = terminate(pid)
	if waitForProcessExit(pid, headlessCancellationGrace) {
		return nil
	}
	_ = killProcess(pid)
	if waitForProcessExit(pid, headlessCancellationGrace) {
		return nil
	}
	return errors.New("headless process did not stop after cancellation")
}

// terminate sends a graceful signal to the command's process group and then
// falls back to the direct child when a platform does not expose the group.
func terminate(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killProcess forcefully terminates a command's process group and falls back
// to the direct child on platforms without process-group support.
func killProcess(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// waitForProcessExit polls until the process disappears or the supplied grace
// period expires.
func waitForProcessExit(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(headlessCancellationPoll)
	}
}

// waitForCommandExit waits for a supervisor child and escalates when it ignores
// SIGTERM, keeping the helper itself responsive to cancellation.
func waitForCommandExit(wait <-chan error, pid int) error {
	timer := time.NewTimer(headlessCancellationGrace)
	defer timer.Stop()
	select {
	case err := <-wait:
		return err
	case <-timer.C:
		_ = killProcess(pid)
		return <-wait
	}
}

// exitCode maps command wait failures to a stable shell-like code.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 128
	}
	return 1
}

// boundedFileWriter drains a native process stream while retaining its head
// and tail, preventing a runaway process from blocking on a pipe.
type boundedFileWriter struct {
	file      *os.File
	limit     int
	head      []byte
	tail      []byte
	total     int
	truncated bool
	finalized bool
}

// Write drains the complete native stream while retaining its head and tail,
// so a late failure event remains inspectable after the capture bound is hit.
func (w *boundedFileWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if w.limit <= 0 {
		w.total += originalLength
		w.truncated = w.total > w.limit
		return originalLength, nil
	}
	headLimit := boundedHeadLimit(w.limit)
	if remaining := headLimit - len(w.head); remaining > 0 {
		count := remaining
		if count > len(data) {
			count = len(data)
		}
		w.head = append(w.head, data[:count]...)
		if _, err := w.file.Write(data[:count]); err != nil {
			return originalLength, err
		}
		data = data[count:]
	}
	tailLimit := w.limit - headLimit
	if len(data) > 0 && tailLimit > 0 {
		w.tail = append(w.tail, data...)
		if len(w.tail) > tailLimit {
			w.tail = append([]byte(nil), w.tail[len(w.tail)-tailLimit:]...)
		}
	}
	w.total += originalLength
	w.truncated = w.total > w.limit
	return originalLength, nil
}

// finalize publishes the retained head and tail to the state file after the
// child exits. The file remains bounded while the pipe is being drained.
func (w *boundedFileWriter) finalize() error {
	if w.finalized || w.file == nil {
		return nil
	}
	w.finalized = true
	if _, err := w.file.Seek(0, 0); err != nil {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	marker := []byte(outputTruncationMarker)
	if w.truncated && w.limit <= len(marker) {
		if w.limit <= 0 {
			return nil
		}
		_, err := w.file.Write(marker[:w.limit])
		return err
	}
	if _, err := w.file.Write(w.head); err != nil {
		return err
	}
	tail := w.tail
	if w.truncated {
		if w.limit <= 0 {
			return nil
		}
		if _, err := w.file.Write(marker); err != nil {
			return err
		}
		tailLimit := w.limit - len(w.head) - len(marker)
		if tailLimit < 0 {
			tailLimit = 0
		}
		if len(tail) > tailLimit {
			tail = tail[len(tail)-tailLimit:]
		}
	}
	_, err := w.file.Write(tail)
	return err
}

// boundedHeadLimit leaves enough room for the truncation marker when a small
// capture limit is used. Normal production limits still split retained bytes
// evenly between the head and tail.
func boundedHeadLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit <= len(outputTruncationMarker) {
		return limit / 2
	}
	return (limit - len(outputTruncationMarker)) / 2
}
