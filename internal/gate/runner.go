// Package gate runs repository-declared setup and deterministic gates through
// the WorkerRuntime seam.
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

// Phase identifies why a gate suite was evaluated.
type Phase string

const (
	// PhaseBaseline identifies the pre-edit health evaluation.
	PhaseBaseline Phase = "baseline"
	// PhaseCheckpoint identifies an evaluation of an implementation checkpoint.
	PhaseCheckpoint Phase = "checkpoint"
)

// Outcome identifies the coordinator-owned result of one declared gate.
type Outcome string

const (
	// OutcomePassed means the declared command exited successfully.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means the declared command failed or timed out.
	OutcomeFailed Outcome = "failed"
	// OutcomeError means the worker could not execute the declared command.
	OutcomeError Outcome = "error"
	// OutcomeSkipped means the gate was not run because a prerequisite failed.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeSetupFailed means setup prevented the gate from being evaluated.
	OutcomeSetupFailed Outcome = "setup_failed"
)

// Request selects one setup-and-gate execution against an immutable
// checkpoint. It is retained as the small compatibility seam for callers that
// need one named gate rather than a complete ordered suite.
type Request struct {
	// RunID selects the already-started worker.
	RunID string
	// CheckpointSHA is the exact commit to which the final status is attached.
	CheckpointSHA string
	// Setup is the checked-in repository setup command.
	Setup string
	// SetupEnvironmentPolicy is the checked-in worker environment for setup.
	SetupEnvironmentPolicy config.EnvironmentPolicy
	// SetupTimeout is the checked-in setup timeout.
	SetupTimeout string
	// Gate is the one checked-in gate selected by the coordinator.
	Gate config.GateConfig
	// Phase identifies why this gate was evaluated.
	Phase Phase
	// SetupFingerprint records the configured dependency inputs used for setup.
	SetupFingerprint string
}

// SuiteRequest selects setup and all ordered gates for one immutable checkpoint.
type SuiteRequest struct {
	// RunID selects the already-started worker.
	RunID string
	// CheckpointSHA is the exact commit to which every status is attached.
	CheckpointSHA string
	// Setup is the checked-in repository setup command.
	Setup string
	// SetupEnvironmentPolicy is the checked-in worker environment for setup.
	SetupEnvironmentPolicy config.EnvironmentPolicy
	// SetupTimeout is the checked-in setup timeout.
	SetupTimeout string
	// Gates is the ordered repository-declared gate list.
	Gates []config.GateConfig
	// Phase identifies whether this is a baseline or checkpoint evaluation.
	Phase Phase
	// SetupFingerprint identifies the configured manifest and lockfile contents
	// observed for this evaluation.
	SetupFingerprint string
	// SkipSetup reuses a previously successful setup for the same checkpoint and
	// dependency-input fingerprint.
	SkipSetup bool
}

// Result contains one deterministic gate outcome and its exact checkpoint
// status. Command output remains available to the in-process caller only.
type Result struct {
	// CheckpointSHA is the immutable commit evaluated by the run.
	CheckpointSHA string
	// GateName identifies the declared gate.
	GateName string
	// Phase identifies why the gate was evaluated.
	Phase Phase
	// Blocking records the repository policy for this gate.
	Blocking bool
	// Setup is the setup command result observed before this gate.
	Setup worker.CommandResult
	// Gate is the deterministic gate command result when the gate ran.
	Gate worker.CommandResult
	// SetupRan reports whether this suite executed setup for the checkpoint.
	SetupRan bool
	// SetupFingerprint records the configured dependency inputs used by setup.
	SetupFingerprint string
	// Outcome is the coordinator-owned semantic result.
	Outcome Outcome
	// Skipped reports that the gate command was not executed.
	Skipped bool
	// SkipReason explains a dependency or setup skip without reporting a failure.
	SkipReason string
	// Status is the final exact-SHA Commit Status request.
	Status github.CommitStatus
}

// SuiteResult contains setup and every declared gate result in declaration
// order. It is returned even when one or more blocking gates fail.
type SuiteResult struct {
	// CheckpointSHA is the immutable commit evaluated by the suite.
	CheckpointSHA string
	// Phase identifies why the suite was evaluated.
	Phase Phase
	// Setup is the one setup command result.
	Setup worker.CommandResult
	// SetupRan reports whether setup was executed.
	SetupRan bool
	// SetupFingerprint records the configured dependency inputs used by setup.
	SetupFingerprint string
	// Gates contains one result for every declared gate.
	Gates []Result

	runID string
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
	if e == nil {
		return "setup failed"
	}
	if e.Cause != nil {
		return fmt.Sprintf("setup execution failed: %v", e.Cause)
	}
	return fmt.Sprintf("setup command failed with exit code %d", e.Result.ExitCode)
}

// Unwrap exposes the setup runtime cause when one exists.
func (e *SetupFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// GateFailure identifies a declared gate command or gate runtime failure.
type GateFailure struct {
	// Name is the declared gate name.
	Name string
	// Result is the gate command result when the worker returned one.
	Result worker.CommandResult
	// Cause is the runtime error when command execution itself failed.
	Cause error
	// TimedOut reports that the configured gate timeout expired.
	TimedOut bool
	// Blocking records whether this failure blocks the suite.
	Blocking bool
}

// Error describes the gate failure without including command output.
func (e *GateFailure) Error() string {
	if e == nil {
		return "gate failed"
	}
	if e.TimedOut {
		return fmt.Sprintf("gate %q timed out", e.Name)
	}
	if e.Cause != nil {
		return fmt.Sprintf("gate %q execution failed: %v", e.Name, e.Cause)
	}
	return fmt.Sprintf("gate %q failed with exit code %d", e.Name, e.Result.ExitCode)
}

// Unwrap exposes the gate runtime cause when one exists.
func (e *GateFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SuiteFailure aggregates all blocking failures from one ordered suite while
// retaining independent gate failures for errors.As and diagnostics.
type SuiteFailure struct {
	// Failures contains setup or blocking gate failures in evaluation order.
	Failures []error
}

// Error describes the complete deterministic failure set without command
// output, which remains in the structured in-process results.
func (e *SuiteFailure) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "gate suite failed"
	}
	parts := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		if failure != nil {
			parts = append(parts, failure.Error())
		}
	}
	return "gate suite failed: " + strings.Join(parts, "; ")
}

// Unwrap exposes every retained failure to errors.Is and errors.As.
func (e *SuiteFailure) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.Failures
}

// Runner executes setup and declared gates using a worker and publishes exact
// checkpoint statuses. It never asks an agent to interpret command output.
type Runner struct {
	// Runtime is the portable worker execution seam.
	Runtime worker.WorkerRuntime
	// Repository is the host-side GitHub repository receiving statuses.
	Repository github.Repository
	// Statuses publishes exact-SHA GitHub results.
	Statuses github.CommitStatusPublisher
}

// Run executes setup followed by one declared gate at the compatibility seam.
// New workflow code should use RunSuite so setup is shared across all gates.
func (r Runner) Run(ctx context.Context, request Request) (Result, error) {
	phase := request.Phase
	if phase == "" {
		phase = PhaseCheckpoint
	}
	suite, err := r.RunSuite(ctx, SuiteRequest{
		RunID:                  request.RunID,
		CheckpointSHA:          request.CheckpointSHA,
		Setup:                  request.Setup,
		SetupEnvironmentPolicy: request.SetupEnvironmentPolicy,
		SetupTimeout:           request.SetupTimeout,
		Gates:                  []config.GateConfig{request.Gate},
		Phase:                  phase,
		SetupFingerprint:       request.SetupFingerprint,
	})
	if len(suite.Gates) == 0 {
		return Result{}, err
	}
	return suite.Gates[0], err
}

// RunSuite executes setup once, then evaluates every declared gate in order.
// Independent gates continue after a failure; gates depending on a failed gate
// receive an explicit skipped result instead of a failure.
func (r Runner) RunSuite(ctx context.Context, request SuiteRequest) (SuiteResult, error) {
	if err := r.validateSuiteRequest(request); err != nil {
		return SuiteResult{}, err
	}
	phase := request.Phase
	if phase == "" {
		phase = PhaseCheckpoint
	}
	result := SuiteResult{
		CheckpointSHA:    request.CheckpointSHA,
		Phase:            phase,
		SetupFingerprint: request.SetupFingerprint,
		Gates:            make([]Result, 0, len(request.Gates)),
		runID:            request.RunID,
	}

	var setupErr error
	if !request.SkipSetup {
		setupContext, cancelSetup := context.WithTimeout(ctx, requestSetupTimeout(request))
		result.SetupRan = true
		result.Setup, setupErr = r.Runtime.RunCommand(setupContext, worker.CommandRequest{
			RunID:             request.RunID,
			Command:           request.Setup,
			EnvironmentPolicy: workerEnvironmentPolicy(request.SetupEnvironmentPolicy),
			Role:              "coordinator",
		})
		cancelSetup()
	}
	if setupErr != nil || result.Setup.ExitCode != 0 {
		failure := &SetupFailure{Result: result.Setup, Cause: setupErr}
		publishFailures := make([]error, 0, len(request.Gates))
		for index, declared := range request.Gates {
			gateResult := r.setupFailedResult(result, declared, index, failure)
			result.Gates = append(result.Gates, gateResult)
			if err := r.publish(ctx, gateResult.Status); err != nil {
				publishFailures = append(publishFailures, err)
			}
		}
		return result, &SuiteFailure{Failures: append([]error{failure}, publishFailures...)}
	}

	failed := make(map[string]bool, len(request.Gates))
	blockingFailures := make([]error, 0)
	for index, declared := range request.Gates {
		if dependency, reason := failedDependency(declared, failed); dependency != "" {
			gateResult := r.skippedResult(result, declared, index, reason)
			result.Gates = append(result.Gates, gateResult)
			if err := r.publish(ctx, gateResult.Status); err != nil {
				blockingFailures = append(blockingFailures, err)
			}
			failed[declared.Name] = true
			continue
		}

		gateResult, failure := r.runDeclaredGate(ctx, result, declared)
		result.Gates = append(result.Gates, gateResult)
		if err := r.publish(ctx, gateResult.Status); err != nil {
			blockingFailures = append(blockingFailures, err)
		}
		if failure != nil {
			failed[declared.Name] = true
			if declared.Blocking {
				blockingFailures = append(blockingFailures, failure)
			}
		}
	}
	if len(blockingFailures) != 0 {
		return result, &SuiteFailure{Failures: blockingFailures}
	}
	return result, nil
}

// validateSuiteRequest validates the public gate execution contract before
// creating a worker command or publishing a status.
func (r Runner) validateSuiteRequest(request SuiteRequest) error {
	if r.Runtime == nil {
		return errors.New("gate runner worker runtime is required")
	}
	if r.Statuses == nil {
		return errors.New("gate runner commit status publisher is required")
	}
	if strings.TrimSpace(request.RunID) == "" {
		return errors.New("gate run id is required")
	}
	if !github.ValidCommitSHA(request.CheckpointSHA) {
		return errors.New("gate checkpoint SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(request.Setup) == "" {
		return errors.New("gate setup command is required")
	}
	if request.SetupEnvironmentPolicy != "" && request.SetupEnvironmentPolicy != config.EnvironmentPolicyClean && request.SetupEnvironmentPolicy != config.EnvironmentPolicyRole {
		return errors.New("gate setup environment policy must be clean or role")
	}
	if _, err := positiveDuration("setup timeout", request.SetupTimeout); err != nil {
		return err
	}
	if request.Phase != "" && request.Phase != PhaseBaseline && request.Phase != PhaseCheckpoint {
		return fmt.Errorf("unsupported gate phase %q", request.Phase)
	}
	if len(request.Gates) == 0 {
		return errors.New("at least one declared gate is required")
	}
	seen := make(map[string]struct{}, len(request.Gates))
	for index, declared := range request.Gates {
		if strings.TrimSpace(declared.Name) == "" {
			return fmt.Errorf("gate %d name is required", index)
		}
		if strings.ContainsAny(declared.Name, "\r\n") {
			return fmt.Errorf("gate %q name must be a single line", declared.Name)
		}
		if _, exists := seen[declared.Name]; exists {
			return fmt.Errorf("gate %q is declared more than once", declared.Name)
		}
		seen[declared.Name] = struct{}{}
		if strings.TrimSpace(declared.Command) == "" {
			return fmt.Errorf("gate %q command is required", declared.Name)
		}
		if _, err := positiveDuration(fmt.Sprintf("gate %q timeout", declared.Name), declared.Timeout); err != nil {
			return err
		}
		if declared.EnvironmentPolicy != config.EnvironmentPolicyClean && declared.EnvironmentPolicy != config.EnvironmentPolicyRole {
			return fmt.Errorf("gate %q environment policy must be clean or role", declared.Name)
		}
		for _, dependency := range declared.DependsOn {
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("gate %q depends on undeclared or later gate %q", declared.Name, dependency)
			}
		}
	}
	return nil
}

// runDeclaredGate executes one gate and maps command and timeout outcomes to a
// typed result and failure without stopping independent gates.
func (r Runner) runDeclaredGate(ctx context.Context, suite SuiteResult, declared config.GateConfig) (Result, error) {
	gateResult := baseResult(suite, declared)
	gateContext, cancelGate := context.WithTimeout(ctx, requestGateTimeout(declared))
	var commandErr error
	gateResult.Gate, commandErr = r.Runtime.RunCommand(gateContext, worker.CommandRequest{
		RunID:             suite.runID,
		Command:           declared.Command,
		EnvironmentPolicy: workerEnvironmentPolicy(declared.EnvironmentPolicy),
		Role:              "gate",
	})
	timedOut := gateContext.Err() == context.DeadlineExceeded
	cancelGate()
	if commandErr == nil && gateResult.Gate.ExitCode == 0 {
		gateResult.Outcome = OutcomePassed
		gateResult.Status.State = github.CommitStatusSuccess
		gateResult.Status.Description = "factory gate passed"
		return gateResult, nil
	}
	timedOut = timedOut || errors.Is(commandErr, context.DeadlineExceeded)
	failure := &GateFailure{Name: declared.Name, Result: gateResult.Gate, Cause: commandErr, TimedOut: timedOut, Blocking: declared.Blocking}
	gateResult.Outcome = OutcomeFailed
	if timedOut {
		gateResult.Status.State = github.CommitStatusFailure
		gateResult.Status.Description = "factory gate timed out"
	} else if commandErr != nil {
		gateResult.Outcome = OutcomeError
		gateResult.Status.State = github.CommitStatusError
		gateResult.Status.Description = "factory gate execution failed"
	} else {
		gateResult.Status.State = github.CommitStatusFailure
		gateResult.Status.Description = "factory gate failed"
	}
	return gateResult, failure
}

// setupFailedResult records every declared gate without misclassifying it as a
// gate failure when setup itself failed.
func (r Runner) setupFailedResult(suite SuiteResult, declared config.GateConfig, _ int, failure *SetupFailure) Result {
	result := baseResult(suite, declared)
	result.Outcome = OutcomeSetupFailed
	result.Skipped = true
	result.SkipReason = "setup failed"
	result.Status.State = github.CommitStatusError
	result.Status.Description = "factory setup failed"
	result.Setup = failure.Result
	return result
}

// skippedResult records a dependency skip as a non-failure status.
func (r Runner) skippedResult(suite SuiteResult, declared config.GateConfig, _ int, reason string) Result {
	result := baseResult(suite, declared)
	result.Outcome = OutcomeSkipped
	result.Skipped = true
	result.SkipReason = reason
	result.Status.State = github.CommitStatusPending
	result.Status.Description = trimDescription("factory gate skipped: " + reason)
	return result
}

// baseResult creates the shared exact-checkpoint result envelope for one gate.
func baseResult(suite SuiteResult, declared config.GateConfig) Result {
	return Result{
		CheckpointSHA:    suite.CheckpointSHA,
		GateName:         declared.Name,
		Phase:            suite.Phase,
		Blocking:         declared.Blocking,
		Setup:            suite.Setup,
		SetupRan:         suite.SetupRan,
		SetupFingerprint: suite.SetupFingerprint,
		Status: github.CommitStatus{
			SHA:     suite.CheckpointSHA,
			Context: statusContext(suite.Phase, declared.Name),
		},
	}
}

// failedDependency returns the first declared prerequisite that failed.
func failedDependency(declared config.GateConfig, failed map[string]bool) (string, string) {
	for _, dependency := range declared.DependsOn {
		if failed[dependency] {
			return dependency, fmt.Sprintf("dependency %q failed", dependency)
		}
	}
	return "", ""
}

// requestSetupTimeout parses the already validated setup timeout.
func requestSetupTimeout(request SuiteRequest) time.Duration {
	duration, _ := time.ParseDuration(strings.TrimSpace(request.SetupTimeout))
	return duration
}

// requestGateTimeout parses the already validated gate timeout.
func requestGateTimeout(declared config.GateConfig) time.Duration {
	duration, _ := time.ParseDuration(strings.TrimSpace(declared.Timeout))
	return duration
}

// publish sends one status and preserves the exact checkpoint identity.
func (r Runner) publish(ctx context.Context, status github.CommitStatus) error {
	if err := r.Statuses.CreateCommitStatus(ctx, r.Repository, status); err != nil {
		return fmt.Errorf("publish gate status: %w", err)
	}
	return nil
}

// positiveDuration parses a trimmed duration string and returns an error unless
// the duration is greater than zero.
func positiveDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	return duration, nil
}

// workerEnvironmentPolicy converts a configured environment policy to the
// corresponding worker policy.
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

// statusContext returns a stable status context for one gate and evaluation
// phase, keeping baseline and checkpoint results from overwriting one another.
func statusContext(phase Phase, name string) string {
	if phase == PhaseBaseline {
		return "factory/baseline/" + strings.TrimSpace(name)
	}
	return "factory/gate/" + strings.TrimSpace(name)
}

// trimDescription keeps generated GitHub status descriptions within the API
// limit while retaining the typed skip reason.
func trimDescription(value string) string {
	if len(value) <= 140 {
		return value
	}
	return value[:137] + "..."
}
