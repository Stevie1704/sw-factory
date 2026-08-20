// Package gate runs the repository-declared setup command and one declared
// deterministic gate through the WorkerRuntime seam.
package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// Request selects one setup-and-gate execution against an immutable
// checkpoint. The repository configuration is supplied by the caller from the
// frozen specification packet rather than re-read from live files.
type Request struct {
	// RunID selects the already-started worker.
	RunID string
	// CheckpointSHA is the exact commit to which the final status is attached.
	CheckpointSHA string
	// Setup is the checked-in repository setup command.
	Setup string
	// SetupTimeout is the checked-in setup timeout.
	SetupTimeout string
	// Gate is the one checked-in gate selected by the coordinator.
	Gate config.GateConfig
}

// Result contains both command outcomes and the exact status that was
// published for the checkpoint.
type Result struct {
	// CheckpointSHA is the immutable commit evaluated by the run.
	CheckpointSHA string
	// GateName identifies the declared gate.
	GateName string
	// Setup is the deterministic setup command result.
	Setup worker.CommandResult
	// Gate is the deterministic gate command result.
	Gate worker.CommandResult
	// Status is the final exact-SHA Commit Status request.
	Status github.CommitStatus
}

// SetupFailure identifies a setup command or setup runtime failure separately
// from a declared gate failure.
type SetupFailure struct {
	// Result is the setup command result when the worker returned one.
	Result worker.CommandResult
	// Cause is the runtime error when command execution itself failed.
	Cause error
}

// Error describes the setup failure without including command output.
func (e *SetupFailure) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("setup execution failed: %v", e.Cause)
	}
	return fmt.Sprintf("setup command failed with exit code %d", e.Result.ExitCode)
}

// Unwrap exposes the setup runtime cause when one exists.
func (e *SetupFailure) Unwrap() error { return e.Cause }

// GateFailure identifies a declared gate command or gate runtime failure.
type GateFailure struct {
	// Name is the declared gate name.
	Name string
	// Result is the gate command result when the worker returned one.
	Result worker.CommandResult
	// Cause is the runtime error when command execution itself failed.
	Cause error
}

// Error describes the gate failure without including command output.
func (e *GateFailure) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("gate %q execution failed: %v", e.Name, e.Cause)
	}
	return fmt.Sprintf("gate %q failed with exit code %d", e.Name, e.Result.ExitCode)
}

// Unwrap exposes the gate runtime cause when one exists.
func (e *GateFailure) Unwrap() error { return e.Cause }

// Runner executes setup and one declared gate using a worker and publishes
// one stable Commit Status context for the exact checkpoint.
type Runner struct {
	// Runtime is the portable worker execution seam.
	Runtime worker.WorkerRuntime
	// Repository is the host-side GitHub repository receiving statuses.
	Repository github.Repository
	// Statuses publishes the exact-SHA GitHub result.
	Statuses github.CommitStatusPublisher
}

// Run executes setup first, then the selected gate, and publishes a final
// status even when a command exits unsuccessfully. It never asks an agent to
// interpret command output.
func (r Runner) Run(ctx context.Context, request Request) (Result, error) {
	if r.Runtime == nil {
		return Result{}, errors.New("gate runner worker runtime is required")
	}
	if r.Statuses == nil {
		return Result{}, errors.New("gate runner commit status publisher is required")
	}
	if strings.TrimSpace(request.RunID) == "" {
		return Result{}, errors.New("gate run id is required")
	}
	if !validCheckpoint(request.CheckpointSHA) {
		return Result{}, errors.New("gate checkpoint SHA must contain 40 to 64 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(request.Setup) == "" {
		return Result{}, errors.New("gate setup command is required")
	}
	setupTimeout, err := positiveDuration("setup timeout", request.SetupTimeout)
	if err != nil {
		return Result{}, err
	}
	gateTimeout, err := positiveDuration("gate timeout", request.Gate.Timeout)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Gate.Name) == "" {
		return Result{}, errors.New("gate name is required")
	}
	if strings.ContainsAny(request.Gate.Name, "\r\n") {
		return Result{}, errors.New("gate name must be a single line")
	}
	if strings.TrimSpace(request.Gate.Command) == "" {
		return Result{}, errors.New("gate command is required")
	}
	if request.Gate.EnvironmentPolicy != config.EnvironmentPolicyClean && request.Gate.EnvironmentPolicy != config.EnvironmentPolicyRole {
		return Result{}, errors.New("gate environment policy must be clean or role")
	}

	result := Result{
		CheckpointSHA: request.CheckpointSHA,
		GateName:      request.Gate.Name,
		Status: github.CommitStatus{
			SHA:     request.CheckpointSHA,
			Context: statusContext(request.Gate.Name),
		},
	}
	setupContext, cancelSetup := context.WithTimeout(ctx, setupTimeout)
	result.Setup, err = r.Runtime.RunCommand(setupContext, worker.CommandRequest{
		RunID:             request.RunID,
		Command:           request.Setup,
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Role:              "coordinator",
	})
	cancelSetup()
	if err != nil || result.Setup.ExitCode != 0 {
		failure := &SetupFailure{Result: result.Setup, Cause: err}
		result.Status.State = github.CommitStatusError
		result.Status.Description = "factory setup failed"
		return result, errors.Join(failure, r.publish(ctx, result.Status))
	}

	gateContext, cancelGate := context.WithTimeout(ctx, gateTimeout)
	result.Gate, err = r.Runtime.RunCommand(gateContext, worker.CommandRequest{
		RunID:             request.RunID,
		Command:           request.Gate.Command,
		EnvironmentPolicy: workerEnvironmentPolicy(request.Gate.EnvironmentPolicy),
		Role:              "gate",
	})
	cancelGate()
	if err != nil || result.Gate.ExitCode != 0 {
		failure := &GateFailure{Name: request.Gate.Name, Result: result.Gate, Cause: err}
		if err != nil {
			result.Status.State = github.CommitStatusError
			result.Status.Description = "factory gate execution failed"
		} else {
			result.Status.State = github.CommitStatusFailure
			result.Status.Description = "factory gate failed"
		}
		return result, errors.Join(failure, r.publish(ctx, result.Status))
	}

	result.Status.State = github.CommitStatusSuccess
	result.Status.Description = "factory setup and gate passed"
	return result, r.publish(ctx, result.Status)
}

// publish sends one status and preserves the exact checkpoint identity.
func (r Runner) publish(ctx context.Context, status github.CommitStatus) error {
	if err := r.Statuses.CreateCommitStatus(ctx, r.Repository, status); err != nil {
		return fmt.Errorf("publish gate status: %w", err)
	}
	return nil
}

// positiveDuration parses one checked-in positive duration.
func positiveDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	return duration, nil
}

// workerEnvironmentPolicy keeps the config and worker packages decoupled at
// their shared execution seam.
func workerEnvironmentPolicy(policy config.EnvironmentPolicy) worker.EnvironmentPolicy {
	switch policy {
	case config.EnvironmentPolicyClean:
		return worker.EnvironmentPolicyClean
	case config.EnvironmentPolicyRole:
		return worker.EnvironmentPolicyRole
	default:
		return worker.EnvironmentPolicyClean
	}
}

// statusContext returns the stable status namespace for one declared gate.
func statusContext(name string) string { return "factory/gate/" + strings.TrimSpace(name) }

// validCheckpoint accepts Git object IDs while keeping the GitHub API path
// safe and stable.
func validCheckpoint(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}
