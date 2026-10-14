package gateproof

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/version"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// ReportSchema is the version of the proof report. Increment it when a field
// changes meaning or is removed.
const ReportSchema = 1

// Result is the overall verdict of one proof.
type Result string

const (
	// ResultPassed means setup passed and every gate has an outcome that final
	// readiness accepts.
	ResultPassed Result = "passed"
	// ResultFailed means setup or a gate has an outcome readiness refuses.
	ResultFailed Result = "failed"
	// ResultUnavailable means no proof ran, for example because the policy is
	// invalid or the worker did not start.
	ResultUnavailable Result = "unavailable"
	// ResultInterrupted means the proof was cancelled before it completed.
	ResultInterrupted Result = "interrupted"
)

// Outcome is the result of setup or of one gate.
type Outcome string

const (
	// OutcomePassed means the command exited with status zero.
	OutcomePassed Outcome = "passed"
	// OutcomeFailed means the command exited with another status.
	OutcomeFailed Outcome = "failed"
	// OutcomeTimedOut means the declared timeout stopped the command.
	OutcomeTimedOut Outcome = "timed_out"
	// OutcomeOutputLimitExceeded means the command wrote past the capture limit.
	OutcomeOutputLimitExceeded Outcome = "output_limit_exceeded"
	// OutcomeOutOfMemory means the worker memory limit killed the command.
	OutcomeOutOfMemory Outcome = "out_of_memory"
	// OutcomeTerminationUnconfirmed means a cancelled command may still run.
	OutcomeTerminationUnconfirmed Outcome = "termination_unconfirmed"
	// OutcomeError means the worker could not run the command.
	OutcomeError Outcome = "error"
	// OutcomeInterrupted means the proof was cancelled during the command.
	OutcomeInterrupted Outcome = "interrupted"
	// OutcomeSkipped means a failed dependency or a halted suite skipped the gate.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeSetupFailed means setup failed, so the gate did not run.
	OutcomeSetupFailed Outcome = "setup_failed"
)

// CleanupStatus tells whether a proof removed what it created.
type CleanupStatus string

const (
	// CleanupRemoved means the worker, its storage, and the workspace are gone.
	CleanupRemoved CleanupStatus = "removed"
	// CleanupIncomplete means something remains; Cleanup removes it later.
	CleanupIncomplete CleanupStatus = "incomplete"
)

// outputTailBytes bounds the command output kept per stream in a report.
const outputTailBytes = 2048

// cleanupTimeout bounds the cleanup that runs after a proof, also after an
// interruption.
const cleanupTimeout = 2 * time.Minute

// workspacePrefix starts the name of every proof workspace below the root.
const workspacePrefix = "onboard-proof-"

// lockFileName is the file a running proof locks in its workspace.
const lockFileName = ".lock"

// Report is the result of one proof.
type Report struct {
	// SchemaVersion is ReportSchema.
	SchemaVersion int `json:"schema_version"`
	// Result is the overall verdict.
	Result Result `json:"result"`
	// Repository is the absolute path of the proved checkout.
	Repository string `json:"repository"`
	// ImageReference is the worker image@digest the proof ran in.
	ImageReference string `json:"image_reference,omitempty"`
	// Candidate identifies the tested candidate files.
	Candidate *Candidate `json:"candidate,omitempty"`
	// GitHead is the checkpoint commit of the candidate that the worker saw
	// at /git, on top of the checkout's HEAD.
	GitHead string `json:"git_head,omitempty"`
	// EvidenceID changes when the candidate files, the policy, the history,
	// the worker digest, the Factory build, or the report schema change.
	EvidenceID string `json:"evidence_id,omitempty"`
	// Setup is the setup result, when setup ran.
	Setup *StepReport `json:"setup,omitempty"`
	// Gates holds one result per declared gate, in declaration order.
	Gates []GateReport `json:"gates"`
	// Diagnostics explains why no proof ran.
	Diagnostics []string `json:"diagnostics"`
	// Cleanup tells whether the proof removed what it created.
	Cleanup CleanupReport `json:"cleanup"`
}

// StepReport is the result of one worker command.
type StepReport struct {
	// Outcome classifies the command result.
	Outcome Outcome `json:"outcome"`
	// ExitCode is the command exit status, when the command finished.
	ExitCode int `json:"exit_code"`
	// StdoutTail and StderrTail hold the end of each output stream.
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// GateReport is the result of one declared gate.
type GateReport struct {
	// Name is the declared gate name.
	Name string `json:"name"`
	// Blocking is the declared gate policy.
	Blocking bool `json:"blocking"`
	// Accepted mirrors final readiness: a pass, or an advisory command
	// failure or timeout.
	Accepted bool `json:"accepted"`
	// SkipReason explains a skip.
	SkipReason string `json:"skip_reason,omitempty"`
	StepReport
}

// CleanupReport tells whether the proof removed what it created.
type CleanupReport struct {
	// Status is removed or incomplete.
	Status CleanupStatus `json:"status"`
	// Workspace is the remaining directory when cleanup is incomplete.
	Workspace string `json:"workspace,omitempty"`
	// Message explains an incomplete cleanup.
	Message string `json:"message,omitempty"`
}

// Verifier runs proofs in workers of Runtime, with workspaces below Root.
type Verifier struct {
	// Runtime starts, runs, and removes proof workers.
	Runtime worker.CleanupRuntime
	// Root holds the proof workspaces. The Docker daemon must be able to
	// bind-mount it.
	Root string
}

// proof is the state of one proof run.
type proof struct {
	// runID names the worker and the workspace.
	runID string
	// workspace holds the candidate copy, Git metadata, and caches.
	workspace string
	// lock marks the workspace as in use until cleanup.
	lock *os.File
	// runtime records every command result for the report.
	runtime *recordingRuntime
}

// Verify proves the candidate in the Git checkout at repository: it
// projects the candidate files, loads the projected factory.yaml, and runs
// setup and every gate in the pinned worker with the production gate runner.
// It publishes no status, claims no issue, and starts no agent.
func (v Verifier) Verify(ctx context.Context, repository string) Report {
	report := Report{SchemaVersion: ReportSchema, Repository: repository, Gates: []GateReport{}, Diagnostics: []string{}}
	run, err := v.newProof()
	if err != nil {
		report = unavailable(report, err.Error())
		report.Cleanup = CleanupReport{Status: CleanupRemoved}
		return report
	}
	report = v.run(ctx, run, repository, report)
	if ctx.Err() != nil {
		report.Result = ResultInterrupted
	}
	report.Cleanup = v.finish(ctx, run)
	return report
}

// run projects the candidate, starts the worker, and runs the suite.
func (v Verifier) run(ctx context.Context, run *proof, repository string, report Report) Report {
	work := filepath.Join(run.workspace, "work")
	gitMetadata := filepath.Join(run.workspace, "git")
	for _, directory := range []string{work, gitMetadata} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return unavailable(report, err.Error())
		}
	}
	candidate, err := Project(ctx, repository, work)
	if err != nil {
		return unavailable(report, err.Error())
	}
	report.Candidate = &candidate
	policy, err := config.LoadRepository(filepath.Join(work, config.RepositoryConfigFileName))
	if err != nil {
		return unavailable(report, policyDiagnostics(filepath.Join(work, config.RepositoryConfigFileName))...)
	}
	report.ImageReference = policy.WorkerBuild.Image + "@" + policy.WorkerBuild.Digest
	report.GitHead, err = checkpointCandidate(ctx, repository, work, gitMetadata)
	if err != nil {
		return unavailable(report, "project the Git metadata: "+err.Error())
	}
	report.EvidenceID = evidenceID(report.ImageReference, candidate.Fingerprint, report.GitHead)
	caches, err := disposableCaches(run.workspace, policy.Caches)
	if err != nil {
		return unavailable(report, err.Error())
	}
	if err := run.runtime.Start(ctx, worker.StartRequest{
		RunID:           run.runID,
		WorktreePath:    work,
		GitMetadataPath: gitMetadata,
		Image:           policy.WorkerBuild.Image,
		ImageDigest:     policy.WorkerBuild.Digest,
		Caches:          caches,
		Role:            "gate",
		Limits:          config.EffectiveWorkerLimits(config.WorkerLimitsConfig{}),
	}); err != nil {
		return unavailable(report, "start the worker: "+err.Error())
	}
	suite, suiteErr := gate.Runner{Runtime: run.runtime, Statuses: discardStatuses{}}.RunSuite(ctx, gate.SuiteRequest{
		RunID:                  run.runID,
		CheckpointSHA:          strings.TrimPrefix(candidate.Fingerprint, "sha256:"),
		Setup:                  policy.Setup,
		SetupEnvironmentPolicy: policy.SetupEnvironmentPolicy,
		SetupTimeout:           policy.Timeouts.Setup,
		Gates:                  policy.Gates,
		Phase:                  gate.PhaseCheckpoint,
	})
	if len(suite.Gates) == 0 && suiteErr != nil {
		return unavailable(report, suiteErr.Error())
	}
	return summarize(ctx, report, suite, run.runtime.calls())
}

// newProof creates and locks a new workspace below the root. It builds the
// workspace under a hidden name and renames it after it holds the lock, so a
// concurrent Cleanup never sees an unlocked new workspace.
func (v Verifier) newProof() (*proof, error) {
	if v.Runtime == nil {
		return nil, errors.New("a worker runtime is required")
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("create a proof identity: %w", err)
	}
	runID := workspacePrefix + hex.EncodeToString(suffix)
	if err := os.MkdirAll(v.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create the proof root: %w", err)
	}
	hidden := filepath.Join(v.Root, "."+runID)
	if err := os.Mkdir(hidden, 0o700); err != nil {
		return nil, fmt.Errorf("create the proof workspace: %w", err)
	}
	lock, err := lockWorkspace(hidden)
	if err != nil {
		_ = os.RemoveAll(hidden)
		return nil, err
	}
	workspace := filepath.Join(v.Root, runID)
	if err := os.Rename(hidden, workspace); err != nil {
		_ = lock.Close()
		_ = os.RemoveAll(hidden)
		return nil, fmt.Errorf("publish the proof workspace: %w", err)
	}
	return &proof{runID: runID, workspace: workspace, lock: lock, runtime: &recordingRuntime{WorkerRuntime: v.Runtime}}, nil
}

// finish removes the worker, its storage, and the workspace. It runs with
// its own deadline, so an interrupted proof still cleans up.
func (v Verifier) finish(ctx context.Context, run *proof) CleanupReport {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	err := removeWorkspace(cleanupContext, v.Runtime, run.runID, run.workspace, run.lock)
	if err != nil {
		return CleanupReport{Status: CleanupIncomplete, Workspace: run.workspace, Message: err.Error() + "; run factory onboard verify --cleanup --json"}
	}
	return CleanupReport{Status: CleanupRemoved}
}

// purgeCommand removes everything the worker can reach in its writable
// mounts, but not the mount points themselves.
const purgeCommand = "find /work -mindepth 1 -delete; find /cache -mindepth 2 -delete; true"

// removeWorkspace removes the worker of runID, its role storage, and the
// workspace directory, then releases the lock.
func removeWorkspace(ctx context.Context, runtime worker.CleanupRuntime, runID, workspace string, lock *os.File) error {
	defer func() { _ = lock.Close() }()
	// Files the worker wrote belong to its user, which the host user may not
	// be able to remove, so a worker that still runs removes them first.
	_, _ = runtime.RunCommand(ctx, worker.CommandRequest{RunID: runID, Command: purgeCommand, EnvironmentPolicy: worker.EnvironmentPolicyClean, Role: "coordinator"})
	if err := runtime.Cleanup(ctx, worker.CleanupRequest{RunID: runID, Roles: []string{"gate"}}); err != nil {
		return fmt.Errorf("remove the proof worker: %w", err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		return fmt.Errorf("remove the proof workspace: %w", err)
	}
	return nil
}

// lockWorkspace opens or creates the lock file of workspace and locks it. It
// fails at once when another process holds the lock.
func lockWorkspace(workspace string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(workspace, lockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the proof lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock the proof workspace: %w", err)
	}
	return file, nil
}

// disposableCaches creates one empty directory per declared cache in the
// workspace. A proof has no host registration, so it runs with cold caches.
func disposableCaches(workspace string, declared []config.CacheConfig) ([]worker.CacheMount, error) {
	caches := make([]worker.CacheMount, 0, len(declared))
	for _, cache := range declared {
		hostPath := filepath.Join(workspace, "caches", cache.Name)
		if err := os.MkdirAll(hostPath, 0o700); err != nil {
			return nil, fmt.Errorf("create the disposable cache %q: %w", cache.Name, err)
		}
		caches = append(caches, worker.CacheMount{Name: cache.Name, HostPath: hostPath, ReadOnly: cache.ReadOnly})
	}
	return caches, nil
}

// evidenceID derives the identity of one proof from the report schema, the
// Factory build, the worker image, the candidate files, which include
// factory.yaml, and the checkpoint commit, which includes the history.
func evidenceID(imageReference, fingerprint, gitHead string) string {
	build := version.Current()
	digest := sha256.Sum256([]byte(fmt.Sprintf("gateproof/%d\x00%s\x00%s\x00%s\x00%s\x00%s", ReportSchema, build.Version, build.Revision, imageReference, fingerprint, gitHead)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// policyDiagnostics describes an invalid projected factory.yaml without
// echoing its values.
func policyDiagnostics(path string) []string {
	diagnosis := config.DiagnoseRepositoryFile(path)
	messages := make([]string, 0, len(diagnosis.Diagnostics))
	for _, diagnostic := range diagnosis.Diagnostics {
		message := "factory.yaml: " + string(diagnostic.Code)
		if diagnostic.Field != "" {
			message += ": " + diagnostic.Field
		}
		messages = append(messages, message+": "+diagnostic.Message+"; run factory config validate for details")
	}
	return messages
}

// unavailable marks a report as a proof that did not run.
func unavailable(report Report, diagnostics ...string) Report {
	report.Result = ResultUnavailable
	report.Diagnostics = append(report.Diagnostics, diagnostics...)
	return report
}

// summarize maps the suite and the recorded commands to the report.
func summarize(ctx context.Context, report Report, suite gate.SuiteResult, calls []commandCall) Report {
	next := 0
	if suite.SetupRan && next < len(calls) {
		setup := stepReport(calls[next])
		report.Setup = &setup
		next++
	}
	for _, result := range suite.Gates {
		gateReport := GateReport{Name: result.GateName, Blocking: result.Blocking, SkipReason: result.SkipReason}
		switch result.Outcome {
		case gate.OutcomeSkipped:
			gateReport.Outcome = OutcomeSkipped
		case gate.OutcomeSetupFailed:
			gateReport.Outcome = OutcomeSetupFailed
		default:
			if next < len(calls) {
				gateReport.StepReport = stepReport(calls[next])
				next++
			} else {
				gateReport.Outcome = OutcomeError
			}
		}
		gateReport.Accepted = accepted(gateReport)
		report.Gates = append(report.Gates, gateReport)
	}
	report.Result = verdict(ctx, report)
	return report
}

// stepReport classifies one recorded worker command.
func stepReport(call commandCall) StepReport {
	return StepReport{
		Outcome:    commandOutcome(call),
		ExitCode:   call.result.ExitCode,
		StdoutTail: tail(call.result.Stdout),
		StderrTail: tail(call.result.Stderr),
	}
}

// commandOutcome maps a command result and error to an outcome.
func commandOutcome(call commandCall) Outcome {
	var termination *worker.CommandTerminationError
	var limit *worker.OutputLimitExceededError
	var memory *worker.OutOfMemoryError
	switch {
	case call.err == nil && call.result.ExitCode == 0:
		return OutcomePassed
	case call.err == nil:
		return OutcomeFailed
	case errors.As(call.err, &termination):
		return OutcomeTerminationUnconfirmed
	case errors.As(call.err, &limit):
		return OutcomeOutputLimitExceeded
	case errors.As(call.err, &memory):
		return OutcomeOutOfMemory
	case errors.Is(call.err, context.DeadlineExceeded):
		return OutcomeTimedOut
	case errors.Is(call.err, context.Canceled):
		return OutcomeInterrupted
	default:
		return OutcomeError
	}
}

// accepted applies the final-readiness rule of production: every gate must
// pass, except that an advisory gate may fail or time out.
func accepted(report GateReport) bool {
	if report.Outcome == OutcomePassed {
		return true
	}
	return !report.Blocking && (report.Outcome == OutcomeFailed || report.Outcome == OutcomeTimedOut)
}

// verdict derives the overall result: passed only when setup passed and
// final readiness accepts every gate.
func verdict(ctx context.Context, report Report) Result {
	if ctx.Err() != nil {
		return ResultInterrupted
	}
	if report.Setup != nil && report.Setup.Outcome != OutcomePassed {
		return ResultFailed
	}
	for _, gateReport := range report.Gates {
		if !gateReport.Accepted {
			return ResultFailed
		}
	}
	return ResultPassed
}

// tail returns at most the last outputTailBytes of output as valid UTF-8.
func tail(output string) string {
	if len(output) > outputTailBytes {
		output = output[len(output)-outputTailBytes:]
	}
	return strings.ToValidUTF8(output, "")
}

// commandCall is one recorded worker command.
type commandCall struct {
	result worker.CommandResult
	err    error
}

// recordingRuntime records the result and error of every command, because
// the gate runner keeps the cause only for blocking failures.
type recordingRuntime struct {
	worker.WorkerRuntime
	mu       sync.Mutex
	recorded []commandCall
}

// RunCommand runs the command and records its result and error.
func (r *recordingRuntime) RunCommand(ctx context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	result, err := r.WorkerRuntime.RunCommand(ctx, request)
	r.mu.Lock()
	r.recorded = append(r.recorded, commandCall{result: result, err: err})
	r.mu.Unlock()
	return result, err
}

// calls returns the recorded commands in order.
func (r *recordingRuntime) calls() []commandCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]commandCall(nil), r.recorded...)
}

// discardStatuses is the status publisher of a proof: it publishes nothing.
type discardStatuses struct{}

// CreateCommitStatus discards the status.
func (discardStatuses) CreateCommitStatus(context.Context, tracker.Repository, codehost.CommitStatus) error {
	return nil
}

// CleanupResult lists what Cleanup did with each proof workspace it found.
type CleanupResult struct {
	// Removed lists the proofs whose worker and workspace were removed.
	Removed []string `json:"removed"`
	// Active lists the proofs that still run and were left alone.
	Active []string `json:"active"`
	// Failed lists the proofs that could not be removed.
	Failed []CleanupFailure `json:"failed"`
}

// CleanupFailure names one proof that Cleanup could not remove.
type CleanupFailure struct {
	// ID names the proof.
	ID string `json:"id"`
	// Message explains the failure.
	Message string `json:"message"`
}

// Cleanup removes the worker and workspace of every proof below the root that
// no running proof holds, such as one a killed process left behind.
func (v Verifier) Cleanup(ctx context.Context) (CleanupResult, error) {
	result := CleanupResult{Removed: []string{}, Active: []string{}, Failed: []CleanupFailure{}}
	if v.Runtime == nil {
		return result, errors.New("a worker runtime is required")
	}
	entries, err := os.ReadDir(v.Root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read the proof root: %w", err)
	}
	for _, entry := range entries {
		// A hidden workspace is one that a killed proof never published.
		id := strings.TrimPrefix(entry.Name(), ".")
		if !entry.IsDir() || !strings.HasPrefix(id, workspacePrefix) {
			continue
		}
		workspace := filepath.Join(v.Root, entry.Name())
		lock, err := lockWorkspace(workspace)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			result.Active = append(result.Active, id)
			continue
		}
		if err == nil {
			err = removeWorkspace(ctx, v.Runtime, id, workspace, lock)
		}
		if err != nil {
			result.Failed = append(result.Failed, CleanupFailure{ID: id, Message: err.Error()})
			continue
		}
		result.Removed = append(result.Removed, id)
	}
	return result, nil
}
