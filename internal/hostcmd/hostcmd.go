// Package hostcmd runs one host executable, such as git or gh, with a
// deadline, without terminal input, and with a byte limit on each output
// stream. A command that does not stop, or that writes too much output, then
// returns a typed error. It does not block the coordinator or grow its memory
// without a limit.
package hostcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// OutputLimit is the most bytes one command may write to stdout or to stderr.
// The streams are counted independently. The largest expected host output is
// a recursive `git ls-tree` of a large repository or a paginated `gh api`
// listing; 32 MiB leaves a wide margin above both.
const OutputLimit = 32 << 20

// waitDelay is how long Run waits for the output pipes to close after it
// stops a command. A descendant that left the process group can keep a pipe
// open; after this delay Run closes the pipes and returns.
const waitDelay = 5 * time.Second

// Command describes one host executable invocation.
type Command struct {
	// Operation names the command in errors, for example "git fetch". Empty
	// means Name followed by the first argument.
	Operation string
	// Name is the executable, resolved through PATH.
	Name string
	// Args are the command-line arguments after Name.
	Args []string
	// Dir is the working directory. Empty means the coordinator's directory.
	Dir string
	// Stdin is the complete standard input. Nil means an empty input.
	Stdin []byte
	// Unset names inherited variables to remove from the environment. Use it
	// where an empty value has a meaning, as GIT_DIR has for Git.
	Unset []string
	// Env holds KEY=value entries added to the inherited environment. A later
	// entry replaces an inherited entry with the same key.
	Env []string
	// Timeout is the deadline for the complete invocation. It must be positive.
	Timeout time.Duration
}

// Output is the captured output of one completed invocation.
type Output struct {
	// Stdout is the complete standard output.
	Stdout []byte
	// Stderr is the complete standard error.
	Stderr []byte
}

// TimeoutError reports that a host command did not finish before its
// deadline. It is a retryable transport failure: the same command can succeed
// on a later attempt. It intentionally does not match
// context.DeadlineExceeded, because callers read that as their own shutdown.
type TimeoutError struct {
	// Operation names the command, for example "git fetch".
	Operation string
	// Timeout is the deadline that expired.
	Timeout time.Duration
}

// Error reports the operation and its deadline.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s did not finish within %s", e.Operation, e.Timeout)
}

// OutputLimitError reports that a host command wrote past OutputLimit. It
// carries no captured output.
type OutputLimitError struct {
	// Operation names the command, for example "gh api".
	Operation string
	// Stream is the stream that overflowed, either stdout or stderr.
	Stream string
	// Limit is the per-stream byte limit.
	Limit int
}

// Error reports the operation, stream, and limit.
func (e *OutputLimitError) Error() string {
	return fmt.Sprintf("%s exceeded the %d byte %s limit", e.Operation, e.Limit, e.Stream)
}

// Run executes command and returns its output. A caller cancellation returns
// the caller's context error, a deadline expiry returns *TimeoutError, and an
// overflow returns *OutputLimitError without output. A process failure
// returns the captured output together with the exec error, so the caller can
// report stderr. Run stops the command's whole process group, so helper
// processes such as git-remote-https or ssh stop with it.
func Run(ctx context.Context, command Command) (Output, error) {
	stdout := &boundedBuffer{}
	stderr := &boundedBuffer{}
	err := execute(ctx, command, stdout, stderr)
	var timeout *TimeoutError
	if errors.As(err, &timeout) || ctx.Err() != nil {
		return Output{}, err
	}
	switch {
	case stdout.exceeded:
		return Output{}, &OutputLimitError{Operation: operationName(command), Stream: "stdout", Limit: OutputLimit}
	case stderr.exceeded:
		return Output{}, &OutputLimitError{Operation: operationName(command), Stream: "stderr", Limit: OutputLimit}
	}
	return Output{Stdout: stdout.captured.Bytes(), Stderr: stderr.captured.Bytes()}, err
}

// Stream executes command and copies its stdout to destination without a byte
// limit, for output that the caller writes to a file instead of memory.
// Stderr is discarded. The deadline, the process-group stop, and the error
// types are the same as for Run.
func Stream(ctx context.Context, command Command, destination io.Writer) error {
	return execute(ctx, command, destination, io.Discard)
}

// execute runs command with its deadline in its own process group. A caller
// cancellation returns the caller's context error, and a deadline expiry
// returns *TimeoutError. Every other result is the exec error.
func execute(ctx context.Context, command Command, stdout, stderr io.Writer) error {
	if command.Timeout <= 0 {
		return errors.New("host command timeout must be positive")
	}
	deadline, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	process := exec.CommandContext(deadline, command.Name, command.Args...)
	process.Dir = command.Dir
	process.Env = append(inheritedEnvironment(command.Unset), command.Env...)
	process.Stdin = bytes.NewReader(command.Stdin)
	process.Stdout = stdout
	process.Stderr = stderr
	// A separate process group lets execute stop every descendant together,
	// and keeps the command away from the terminal's foreground input.
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error { return syscall.Kill(-process.Process.Pid, syscall.SIGKILL) }
	process.WaitDelay = waitDelay
	err := process.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil && errors.Is(deadline.Err(), context.DeadlineExceeded) {
		return &TimeoutError{Operation: operationName(command), Timeout: command.Timeout}
	}
	return err
}

// inheritedEnvironment returns the coordinator's environment without the
// named variables.
func inheritedEnvironment(unset []string) []string {
	environment := os.Environ()
	if len(unset) == 0 {
		return environment
	}
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(unset, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// operationName returns the declared operation, or names an invocation by its
// executable and first argument. Later arguments can hold paths or repository
// names, so they stay out of error messages.
func operationName(command Command) string {
	if command.Operation != "" {
		return command.Operation
	}
	if len(command.Args) == 0 {
		return command.Name
	}
	return command.Name + " " + command.Args[0]
}

// boundedBuffer keeps at most OutputLimit bytes of one stream and records
// whether the command tried to write more.
type boundedBuffer struct {
	captured bytes.Buffer
	exceeded bool
}

// Write keeps bytes up to the limit and discards the rest. It always reports
// a complete write, so the copy that feeds it keeps draining the pipe and the
// command does not block on a full pipe.
func (b *boundedBuffer) Write(data []byte) (int, error) {
	if remaining := OutputLimit - b.captured.Len(); len(data) > remaining {
		b.exceeded = true
		if remaining > 0 {
			b.captured.Write(data[:remaining])
		}
		return len(data), nil
	}
	return b.captured.Write(data)
}
