package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const headlessStateRoot = "/home/factory/.factory-headless"

// HeadlessLaunchMode identifies whether a headless process is a fresh launch
// or a replacement for a previously lost native process.
type HeadlessLaunchMode string

const (
	// HeadlessLaunchFresh starts a process only when no prior process state
	// exists for the invocation.
	HeadlessLaunchFresh HeadlessLaunchMode = "fresh"
	// HeadlessLaunchResume permits replacement after the coordinator has
	// deliberately selected an exact native-session resume.
	HeadlessLaunchResume HeadlessLaunchMode = "resume"
)

// HeadlessStatus is the worker-owned lifecycle projection for one detached
// process. It contains no Docker name, host PID, or terminal identity.
type HeadlessStatus string

const (
	// HeadlessStatusMissing means no process state has been created yet.
	HeadlessStatusMissing HeadlessStatus = "missing"
	// HeadlessStatusStarting means the helper has reserved the invocation.
	HeadlessStatusStarting HeadlessStatus = "starting"
	// HeadlessStatusRunning means the native process is still alive.
	HeadlessStatusRunning HeadlessStatus = "running"
	// HeadlessStatusExited means the native process completed and its output is
	// available for adapter inspection.
	HeadlessStatusExited HeadlessStatus = "exited"
	// HeadlessStatusCancelled means the worker accepted an idempotent cancel.
	HeadlessStatusCancelled HeadlessStatus = "cancelled"
	// HeadlessStatusLost means durable state says the process was active but no
	// matching helper process remains.
	HeadlessStatusLost HeadlessStatus = "lost"
)

// HeadlessRequest describes one detached command owned by a worker runtime.
// Command arguments remain opaque to the coordinator and execute only inside
// the already isolated worker.
type HeadlessRequest struct {
	// RunID selects the logical run owning the worker.
	RunID string
	// WorkerID selects an invocation-isolated worker when supplied.
	WorkerID string
	// InvocationID selects the durable process state directory.
	InvocationID string
	// Command is the executable and arguments run inside the worker.
	Command []string
	// EnvironmentPolicy selects the clean or role worker environment.
	EnvironmentPolicy EnvironmentPolicy
	// Role identifies the role-owned home and explicit role environment.
	Role string
	// Environment carries only coordinator-owned non-secret protocol values.
	Environment map[string]string
	// Mode controls fresh idempotency or exact-session replacement semantics.
	Mode HeadlessLaunchMode
}

// HeadlessExecution identifies a detached worker process without exposing a
// runtime-specific process identifier.
type HeadlessExecution struct {
	// RunID identifies the owning run.
	RunID string
	// WorkerID identifies the owning worker.
	WorkerID string
	// InvocationID identifies the process state.
	InvocationID string
}

// HeadlessInspection reports bounded detached-process state and diagnostics.
// The harness adapter, not workflow code, interprets the captured streams.
type HeadlessInspection struct {
	// Status is the worker-owned process lifecycle state.
	Status HeadlessStatus
	// ExitCode is meaningful after an exited or cancelled process.
	ExitCode int
	// Stdout is bounded machine-readable process output.
	Stdout string
	// Stderr is bounded diagnostic output.
	Stderr string
	// StdoutTruncated reports that retained stdout is incomplete.
	StdoutTruncated bool
	// StderrTruncated reports that retained stderr is incomplete.
	StderrTruncated bool
}

// HeadlessProcessRuntime is the optional worker extension for terminal-free
// detached execution. It deliberately stays separate from WorkerRuntime so
// existing worker embedders do not acquire process authority accidentally.
type HeadlessProcessRuntime interface {
	WorkerRuntime
	// StartHeadless launches or reuses one detached process in the worker.
	StartHeadless(context.Context, HeadlessRequest) (HeadlessExecution, error)
	// InspectHeadless reads the durable process state and bounded diagnostics.
	InspectHeadless(context.Context, HeadlessRequest) (HeadlessInspection, error)
	// CancelHeadless requests idempotent process cancellation.
	CancelHeadless(context.Context, HeadlessRequest) error
	// FinishHeadless performs idempotent accepted-completion shutdown.
	FinishHeadless(context.Context, HeadlessRequest) error
}

// StartHeadless launches a detached helper process without allocating a TTY or
// attaching the coordinator's standard streams.
func (r *DockerRuntime) StartHeadless(ctx context.Context, request HeadlessRequest) (HeadlessExecution, error) {
	if err := validateHeadlessRequest(request, true); err != nil {
		return HeadlessExecution{}, err
	}
	workerID := workerResourceID(request.RunID, request.WorkerID)
	inspection, err := r.inspectHeadless(ctx, request)
	if err != nil {
		return HeadlessExecution{}, err
	}
	switch inspection.Status {
	case HeadlessStatusStarting, HeadlessStatusRunning:
		// A detached process already owns the invocation, or a resume is still
		// in flight. Both states are idempotent observations across response loss.
		return HeadlessExecution{RunID: request.RunID, WorkerID: workerID, InvocationID: request.InvocationID}, nil
	case HeadlessStatusExited:
		if request.Mode == HeadlessLaunchFresh {
			// A fresh launch is idempotent across coordinator response loss. The
			// durable helper state is the launch boundary, so never create a
			// second native process for the same invocation.
			return HeadlessExecution{RunID: request.RunID, WorkerID: workerID, InvocationID: request.InvocationID}, nil
		}
	case HeadlessStatusCancelled:
		if request.Mode != HeadlessLaunchResume {
			return HeadlessExecution{}, errors.New("headless invocation was already cancelled")
		}
	case HeadlessStatusLost:
		if request.Mode != HeadlessLaunchResume {
			return HeadlessExecution{}, errors.New("headless invocation process was lost")
		}
	case HeadlessStatusMissing:
		if request.Mode == HeadlessLaunchResume {
			return HeadlessExecution{}, errors.New("headless invocation state is unavailable for resume")
		}
	default:
		return HeadlessExecution{}, fmt.Errorf("unsupported headless process state %q", inspection.Status)
	}

	containerInspection, err := r.inspectContainer(ctx, containerName(workerID))
	if err != nil {
		return HeadlessExecution{}, fmt.Errorf("inspect worker before headless launch: %w", err)
	}
	if !containerInspection.Running {
		return HeadlessExecution{}, fmt.Errorf("worker %q is not running", request.RunID)
	}
	environment := commandEnvironment(CommandRequest{
		RunID: request.RunID, WorkerID: request.WorkerID,
		EnvironmentPolicy: request.EnvironmentPolicy, Role: request.Role,
		Environment: request.Environment,
	})
	args := []string{"exec", "-d", "--workdir", WorktreePath}
	for _, value := range environment {
		args = append(args, "--env", value)
	}
	args = append(args, containerName(workerID), "/usr/local/bin/factory-worker-headless", "run", "--state-dir", headlessStatePath(request.InvocationID))
	if request.Mode == HeadlessLaunchResume {
		args = append(args, "--replace")
	}
	args = append(args, "--")
	args = append(args, request.Command...)
	if _, err := r.runDocker(ctx, args); err != nil {
		return HeadlessExecution{}, fmt.Errorf("start headless worker process: %w", err)
	}
	return HeadlessExecution{RunID: request.RunID, WorkerID: workerID, InvocationID: request.InvocationID}, nil
}

// InspectHeadless reads one detached process projection and keeps worker
// helper output bounded before it reaches the harness adapter.
func (r *DockerRuntime) InspectHeadless(ctx context.Context, request HeadlessRequest) (HeadlessInspection, error) {
	if err := validateHeadlessRequest(request, false); err != nil {
		return HeadlessInspection{}, err
	}
	return r.inspectHeadless(ctx, request)
}

// CancelHeadless requests cancellation and treats missing or stopped workers
// as already cancelled, making cleanup safe to replay after a restart.
func (r *DockerRuntime) CancelHeadless(ctx context.Context, request HeadlessRequest) error {
	if err := validateHeadlessRequest(request, false); err != nil {
		return err
	}
	return r.finishHeadless(ctx, request, "cancel")
}

// FinishHeadless is the idempotent accepted-completion shutdown operation.
func (r *DockerRuntime) FinishHeadless(ctx context.Context, request HeadlessRequest) error {
	if err := validateHeadlessRequest(request, false); err != nil {
		return err
	}
	return r.finishHeadless(ctx, request, "finish")
}

// inspectHeadless performs the private Docker/container translation for one
// logical process inspection.
func (r *DockerRuntime) inspectHeadless(ctx context.Context, request HeadlessRequest) (HeadlessInspection, error) {
	workerID := workerResourceID(request.RunID, request.WorkerID)
	containerInspection, err := r.inspectContainer(ctx, containerName(workerID))
	if err != nil {
		if isContainerNotFound(err) {
			return HeadlessInspection{Status: HeadlessStatusMissing}, nil
		}
		return HeadlessInspection{}, fmt.Errorf("inspect worker for headless process: %w", err)
	}
	if !containerInspection.Running {
		return HeadlessInspection{Status: HeadlessStatusLost}, nil
	}
	result, err := r.RunCommand(ctx, CommandRequest{
		RunID: request.RunID, WorkerID: request.WorkerID,
		Command:           "/usr/local/bin/factory-worker-headless inspect --state-dir " + shellQuote(headlessStatePath(request.InvocationID)),
		EnvironmentPolicy: EnvironmentPolicyClean,
	})
	if err != nil {
		return HeadlessInspection{}, fmt.Errorf("inspect headless process state: %w", err)
	}
	if result.ExitCode != 0 {
		return HeadlessInspection{}, errors.New("headless process state inspection failed")
	}
	var wire headlessInspectionWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &wire); err != nil {
		return HeadlessInspection{}, fmt.Errorf("decode headless process state: %w", err)
	}
	return wire.toInspection()
}

// finishHeadless performs the fixed helper cancellation command used by both
// explicit cancellation and accepted-completion cleanup.
func (r *DockerRuntime) finishHeadless(ctx context.Context, request HeadlessRequest, operation string) error {
	workerID := workerResourceID(request.RunID, request.WorkerID)
	containerInspection, err := r.inspectContainer(ctx, containerName(workerID))
	if err != nil {
		if isContainerNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect worker before headless %s: %w", operation, err)
	}
	if !containerInspection.Running {
		return nil
	}
	result, err := r.RunCommand(ctx, CommandRequest{
		RunID: request.RunID, WorkerID: request.WorkerID,
		Command:           "/usr/local/bin/factory-worker-headless cancel --state-dir " + shellQuote(headlessStatePath(request.InvocationID)),
		EnvironmentPolicy: EnvironmentPolicyClean,
	})
	if err != nil {
		return fmt.Errorf("%s headless process: %w", operation, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s headless process returned exit code %s", operation, strconv.Itoa(result.ExitCode))
	}
	return nil
}

// headlessInspectionWire is the bounded JSON protocol emitted by the worker
// helper. Base64 keeps arbitrary native output out of the JSON framing.
type headlessInspectionWire struct {
	// Status is the helper-owned detached process state.
	Status string `json:"status"`
	// ExitCode is the child exit code after termination.
	ExitCode int `json:"exit_code"`
	// Stdout is bounded machine-readable child output encoded as base64.
	Stdout string `json:"stdout"`
	// Stderr is bounded child diagnostic output encoded as base64.
	Stderr string `json:"stderr"`
	// StdoutTruncated reports incomplete retained machine output.
	StdoutTruncated bool `json:"stdout_truncated"`
	// StderrTruncated reports incomplete retained diagnostic output.
	StderrTruncated bool `json:"stderr_truncated"`
}

// toInspection validates the worker helper's neutral wire values.
func (w headlessInspectionWire) toInspection() (HeadlessInspection, error) {
	status := HeadlessStatus(w.Status)
	switch status {
	case HeadlessStatusMissing, HeadlessStatusStarting, HeadlessStatusRunning, HeadlessStatusExited, HeadlessStatusCancelled, HeadlessStatusLost:
	default:
		return HeadlessInspection{}, fmt.Errorf("unknown headless process status %q", w.Status)
	}
	stdout, err := base64.StdEncoding.DecodeString(w.Stdout)
	if err != nil {
		return HeadlessInspection{}, errors.New("headless process stdout is not valid base64")
	}
	stderr, err := base64.StdEncoding.DecodeString(w.Stderr)
	if err != nil {
		return HeadlessInspection{}, errors.New("headless process stderr is not valid base64")
	}
	return HeadlessInspection{Status: status, ExitCode: w.ExitCode, Stdout: string(stdout), Stderr: string(stderr), StdoutTruncated: w.StdoutTruncated, StderrTruncated: w.StderrTruncated}, nil
}

// validateHeadlessRequest validates identity, command, policy, and explicit
// protocol environment before any detached process side effect.
func validateHeadlessRequest(request HeadlessRequest, requireCommand bool) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateOptionalWorkerID(request.WorkerID); err != nil {
		return err
	}
	if err := validateRunID(request.InvocationID); err != nil {
		return fmt.Errorf("headless invocation id: %w", err)
	}
	if requireCommand {
		if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
			return errors.New("headless command is required")
		}
		for index, argument := range request.Command {
			if strings.ContainsAny(argument, "\x00\r") || index == 0 && strings.ContainsRune(argument, '\n') {
				return errors.New("headless command contains control characters")
			}
		}
	}
	if request.EnvironmentPolicy != EnvironmentPolicyClean && request.EnvironmentPolicy != EnvironmentPolicyRole {
		return errors.New("headless environment policy must be clean or role")
	}
	if request.EnvironmentPolicy == EnvironmentPolicyRole {
		if strings.TrimSpace(request.Role) == "" || !validName(request.Role) {
			return errors.New("headless role is required and must be safe")
		}
	} else if request.Role != "" && !validName(request.Role) {
		return errors.New("headless role contains unsafe characters")
	}
	for name, value := range request.Environment {
		if err := validateInteractiveEnvironmentEntry(name, value); err != nil {
			return err
		}
	}
	if request.Mode != "" && request.Mode != HeadlessLaunchFresh && request.Mode != HeadlessLaunchResume {
		return fmt.Errorf("unsupported headless launch mode %q", request.Mode)
	}
	return nil
}

// headlessStatePath returns the fixed worker-local process state path.
func headlessStatePath(invocationID string) string {
	return headlessStateRoot + "/" + invocationID
}

// shellQuote quotes a fixed adapter-owned path for the worker shell used to
// invoke the helper. Invocation IDs have already been restricted to safe
// characters; this function keeps the shell boundary explicit.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

var _ HeadlessProcessRuntime = (*DockerRuntime)(nil)
var _ HeadlessChecker = (*DockerRuntime)(nil)
