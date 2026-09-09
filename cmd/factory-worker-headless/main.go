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
	inspectionBytes   = 512 << 10
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
	Status          string `json:"status"`
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
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
	if !*replace {
		status, err := readStatus(*stateDir)
		if err != nil {
			return err
		}
		if status != statusMissing && status != statusLost {
			return fmt.Errorf("headless process already has state %q", status)
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
	commandProcess.Stdout = &boundedFileWriter{file: stdout, limit: worker.MaxCapturedOutputBytes}
	commandProcess.Stderr = &boundedFileWriter{file: stderr, limit: worker.MaxCapturedOutputBytes}
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

	wait := make(chan error, 1)
	go func() { wait <- commandProcess.Wait() }()
	interrupt := make(chan os.Signal, 1)
	signalNotify(interrupt)
	defer signalStop(interrupt)
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-interrupt:
		_ = writeState(*stateDir, stateCancelMarker, "1")
		_ = terminate(commandProcess.Process.Pid)
		waitErr = <-wait
	}

	if _, err := os.Stat(statePath(*stateDir, stateCancelMarker)); err == nil {
		_ = writeState(*stateDir, stateStatus, string(statusCancelled))
	} else {
		_ = writeState(*stateDir, stateStatus, string(statusExited))
	}
	_ = writeState(*stateDir, stateExitCode, strconv.Itoa(exitCode(waitErr)))
	writer, _ := commandProcess.Stdout.(*boundedFileWriter)
	if writer != nil && writer.truncated {
		_ = writeState(*stateDir, stateStdoutLimit, "1")
	}
	errorWriter, _ := commandProcess.Stderr.(*boundedFileWriter)
	if errorWriter != nil && errorWriter.truncated {
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
	if status == statusStarting || status == statusRunning {
		pid, pidErr := readPID(stateDir)
		if pidErr == nil && processAlive(pid) {
			status = statusRunning
		} else if pidErr == nil {
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

// cancelProcess signals a running detached command and returns once its state
// is terminal or the bounded cancellation wait expires.
func cancelProcess(arguments []string) error {
	stateDir, err := parseStateDir(arguments)
	if err != nil {
		return err
	}
	status, err := readStatus(stateDir)
	if err != nil {
		return err
	}
	if status != statusStarting && status != statusRunning {
		return nil
	}
	pid, err := readPID(stateDir)
	if err != nil {
		_ = writeState(stateDir, stateStatus, string(statusCancelled))
		return nil
	}
	_ = writeState(stateDir, stateCancelMarker, "1")
	if processAlive(pid) {
		_ = terminate(pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = writeState(stateDir, stateStatus, string(statusCancelled))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = writeState(stateDir, stateStatus, string(statusCancelled))
	return nil
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
	return data[:limit], true
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

// terminate sends a graceful signal to the command's process group and then
// falls back to the direct child when a platform does not expose the group.
func terminate(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
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

// boundedFileWriter drains a native process stream while retaining only the
// configured prefix, preventing a runaway process from blocking on a pipe.
type boundedFileWriter struct {
	file      *os.File
	limit     int
	written   int
	truncated bool
}

// Write persists at most the configured prefix and reports a complete write so
// the child process can continue draining output after the bound is reached.
func (w *boundedFileWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.truncated = true
		return originalLength, nil
	}
	if len(data) > remaining {
		w.truncated = true
		data = data[:remaining]
	}
	written, err := w.file.Write(data)
	w.written += written
	return originalLength, err
}

// signalNotify and signalStop keep signal setup isolated for the helper's
// short-lived detached process supervisor.
func signalNotify(channel chan<- os.Signal) {
	signal.Notify(channel, os.Interrupt, syscall.SIGTERM)
}

// signalStop unregisters helper signal delivery.
func signalStop(channel chan<- os.Signal) {
	signal.Stop(channel)
}
