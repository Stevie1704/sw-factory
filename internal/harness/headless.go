package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	headlessDiscoveryTimeout      = 60 * time.Second
	headlessDiscoveryInitialDelay = 100 * time.Millisecond
	headlessDiscoveryMaximumDelay = 500 * time.Millisecond
	maxHeadlessDiagnosticBytes    = 16 << 10
	maxHeadlessEventLineBytes     = 1 << 20
	// headlessCleanupTimeout bounds the cancellation issued after a launch that
	// reached the worker but never produced a usable native identity.
	headlessCleanupTimeout = 30 * time.Second
)

// HeadlessStartRequest contains coordinator-owned identity, prompt, policy,
// and optional exact native resume identity for a terminal-free invocation.
type HeadlessStartRequest struct {
	// InvocationID identifies the report-producing invocation.
	InvocationID string
	// RunID identifies the logical worker run.
	RunID string
	// WorkerID selects the invocation-isolated worker.
	WorkerID string
	// Role identifies the workflow role owning the process.
	Role string
	// Stage identifies the workflow stage.
	Stage string
	// CheckpointSHA binds review prompts to their immutable checkpoint.
	CheckpointSHA string
	// ReviewRoundID identifies the immutable review round, when this invocation
	// belongs to a partitioned review.
	ReviewRoundID string
	// ReviewUnitID identifies the manifest assignment, when this invocation
	// belongs to a partitioned review.
	ReviewUnitID string
	// Prompt is the frozen role prompt.
	Prompt string
	// Model is the repository-selected model.
	Model string
	// ReasoningEffort is the repository-selected reasoning policy.
	ReasoningEffort string
	// ResumeSessionID is the exact native session to continue.
	ResumeSessionID string
}

// HeadlessSession is the opaque identity returned after a headless launch.
type HeadlessSession struct {
	// InvocationID identifies the owning invocation.
	InvocationID string
	// RunID identifies the logical run.
	RunID string
	// WorkerID identifies the logical worker.
	WorkerID string
	// Role identifies the role home used by the process.
	Role string
	// NativeSessionID is the native identity read from a machine-readable event.
	NativeSessionID string
}

// HeadlessInspectionRequest selects one persisted headless process projection.
type HeadlessInspectionRequest struct {
	// InvocationID identifies the durable process state.
	InvocationID string
	// RunID identifies the worker run.
	RunID string
	// WorkerID identifies the invocation-isolated worker.
	WorkerID string
	// Role supplies the explicit worker environment policy.
	Role string
}

// HeadlessInspection reports the adapter-neutral process state and bounded
// machine output used only for control and diagnostics.
type HeadlessInspection struct {
	// Status is the worker-owned detached process state.
	Status worker.HeadlessStatus
	// ExitCode is meaningful after process termination.
	ExitCode int
	// Stdout is bounded machine-readable harness output.
	Stdout string
	// Stderr is bounded diagnostic output.
	Stderr string
	// StdoutTruncated reports incomplete machine output.
	StdoutTruncated bool
	// StderrTruncated reports incomplete diagnostic output.
	StderrTruncated bool
}

// HeadlessRuntime is the factory-owned lifecycle seam for terminal-free
// harnesses. It contains no workspace, surface, keystroke, or screen concept.
type HeadlessRuntime interface {
	Runtime
	// InspectHeadless reads process state and bounded machine output.
	InspectHeadless(context.Context, HeadlessInspectionRequest) (HeadlessInspection, error)
	// CancelHeadless requests idempotent process cancellation.
	CancelHeadless(context.Context, HeadlessSession) error
	// FinishHeadless performs idempotent accepted-completion shutdown.
	FinishHeadless(context.Context, HeadlessSession) error
}

// headlessProtocol carries the only harness-specific parts of a terminal-free
// lifecycle: the adapter identity used by typed outcomes, the machine event
// that reveals the native session, and the event classification that maps a
// native failure onto the factory's existing vocabulary. Everything else in a
// headless launch is shared, so adapters compose this instead of repeating the
// worker protocol.
type headlessProtocol struct {
	// name is the harness identity recorded on an invocation.
	name string
	// nativeSessionID extracts the native identity from machine output and
	// returns an empty string while no such event has been observed.
	nativeSessionID func(output string) string
	// classify maps machine-readable native events to a typed failure and
	// returns nil while nothing in the output names a failure.
	classify func(output string) *HeadlessFailure
	// environment adds adapter-owned non-secret process settings to the
	// coordinator's invocation identity values.
	environment map[string]string
}

// headless owns the adapter-neutral half of HeadlessRuntime. It never starts a
// harness process on the coordinator host; every operation is one call into
// the worker's detached process extension.
type headless struct {
	// worker owns Docker translation, process state, and bounded capture.
	worker worker.HeadlessProcessRuntime
	// protocol supplies the harness-specific command output vocabulary.
	protocol headlessProtocol
}

// InspectHeadless reads the worker-owned detached process projection.
func (h *headless) InspectHeadless(ctx context.Context, request HeadlessInspectionRequest) (HeadlessInspection, error) {
	if h.worker == nil {
		return HeadlessInspection{}, errors.New("worker runtime does not support headless processes")
	}
	result, err := h.worker.InspectHeadless(ctx, worker.HeadlessRequest{
		RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID,
		EnvironmentPolicy: worker.EnvironmentPolicyClean,
		Mode:              worker.HeadlessLaunchFresh,
	})
	if err != nil {
		return HeadlessInspection{}, err
	}
	return HeadlessInspection{Status: result.Status, ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr, StdoutTruncated: result.StdoutTruncated, StderrTruncated: result.StderrTruncated}, nil
}

// CancelHeadless delegates idempotent cancellation to the worker process
// owner, preserving the logical identity needed after coordinator restart.
func (h *headless) CancelHeadless(ctx context.Context, session HeadlessSession) error {
	if h.worker == nil {
		return errors.New("worker runtime does not support headless processes")
	}
	return h.worker.CancelHeadless(ctx, h.workerRequestForSession(session))
}

// FinishHeadless delegates accepted-completion shutdown to the worker and is
// safe to replay after a response-loss boundary.
func (h *headless) FinishHeadless(ctx context.Context, session HeadlessSession) error {
	if h.worker == nil {
		return errors.New("worker runtime does not support headless processes")
	}
	return h.worker.FinishHeadless(ctx, h.workerRequestForSession(session))
}

// NativeSessionID returns the native identity recorded in the durable detached
// process output. The worker remains the only component that reads the process
// state or role home, and the process-associated event prevents a coordinator
// restart from accidentally adopting an older role-home session.
func (h *headless) NativeSessionID(ctx context.Context, request NativeSessionRequest) (string, error) {
	if err := h.expectHarness(request.Harness); err != nil {
		return "", err
	}
	inspection, err := h.InspectHeadless(ctx, HeadlessInspectionRequest{
		InvocationID: request.InvocationID,
		RunID:        request.RunID,
		WorkerID:     request.WorkerID,
	})
	if err != nil {
		return "", err
	}
	if nativeID := h.protocol.nativeSessionID(inspection.Stdout); nativeID != "" {
		return nativeID, nil
	}
	return "", fmt.Errorf("%s headless process has no native session event", h.protocol.name)
}

// NativeSessionRunning maps the durable detached process state to the
// coordinator's liveness observation without inspecting terminal output.
func (h *headless) NativeSessionRunning(ctx context.Context, request NativeSessionRequest) (bool, error) {
	if err := h.expectHarness(request.Harness); err != nil {
		return false, err
	}
	inspection, err := h.InspectHeadless(ctx, HeadlessInspectionRequest{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID})
	if err != nil {
		return false, err
	}
	switch inspection.Status {
	case worker.HeadlessStatusStarting, worker.HeadlessStatusRunning:
		return true, nil
	case worker.HeadlessStatusMissing, worker.HeadlessStatusExited, worker.HeadlessStatusCancelled, worker.HeadlessStatusLost:
		return false, nil
	default:
		return false, fmt.Errorf("unknown %s headless process state %q", h.protocol.name, inspection.Status)
	}
}

// HeadlessFailureFor classifies a terminal detached-process inspection after
// the native session identity is known. It closes the post-launch gap where a
// failure event arrives after the launch polling window.
func (h *headless) HeadlessFailureFor(ctx context.Context, request HeadlessInspectionRequest) error {
	inspection, err := h.InspectHeadless(ctx, request)
	if err != nil {
		return normalizeHeadlessError(h.protocol.name, err)
	}
	if inspection.Status == worker.HeadlessStatusMissing || inspection.Status == worker.HeadlessStatusStarting || inspection.Status == worker.HeadlessStatusRunning {
		return nil
	}
	if failure := h.classifyInspection(inspection); failure != nil {
		failure.Diagnostics = BoundedHeadlessDiagnostics(inspection)
		return failure
	}
	switch inspection.Status {
	case worker.HeadlessStatusCancelled:
		return &HeadlessFailure{Cause: context.Canceled, ExitCode: inspection.ExitCode, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
	case worker.HeadlessStatusExited, worker.HeadlessStatusLost:
		// Preserve the process code for local diagnostics without putting it in
		// the redacted coordinator-visible error category.
		return &HeadlessFailure{Cause: NewUnexpectedExitError(h.protocol.name), ExitCode: inspection.ExitCode, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
	default:
		return fmt.Errorf("unknown %s headless process state %q", h.protocol.name, inspection.Status)
	}
}

// launch runs one already-translated native command in the detached worker and
// returns only after the harness has revealed its native session identity.
// expectedSessionID is the identity the caller requires, empty when the harness
// assigns one that the adapter must discover.
func (h *headless) launch(ctx context.Context, request HeadlessStartRequest, command []string, mode worker.HeadlessLaunchMode, expectedSessionID string) (HeadlessSession, error) {
	if err := validateHeadlessStartRequest(h.protocol.name, request); err != nil {
		return HeadlessSession{}, err
	}
	if h.worker == nil {
		return HeadlessSession{}, errors.New("worker runtime does not support headless processes")
	}
	environment := invocationEnvironment(h.protocol.name, HeadlessStartRequest{
		InvocationID: request.InvocationID, RunID: request.RunID, Role: request.Role,
		Stage: request.Stage, CheckpointSHA: request.CheckpointSHA,
		ReviewRoundID: request.ReviewRoundID, ReviewUnitID: request.ReviewUnitID,
		Model: request.Model,
	})
	if request.ReasoningEffort != "" {
		environment["FACTORY_REASONING_EFFORT"] = request.ReasoningEffort
	}
	for name, value := range h.protocol.environment {
		environment[name] = value
	}
	_, err := h.worker.StartHeadless(ctx, worker.HeadlessRequest{
		RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID,
		Command: command, EnvironmentPolicy: worker.EnvironmentPolicyRole, Role: request.Role,
		Environment: environment, Mode: mode,
	})
	if err != nil {
		return HeadlessSession{}, normalizeHeadlessError(h.protocol.name, err)
	}
	nativeID, err := h.waitForNativeSession(ctx, request)
	if err != nil {
		h.cancelAfterLaunchFailure(ctx, request, nativeID)
		return HeadlessSession{}, err
	}
	if expectedSessionID != "" && nativeID != expectedSessionID {
		h.cancelAfterLaunchFailure(ctx, request, nativeID)
		return HeadlessSession{}, &HeadlessFailure{Cause: NewUnexpectedExitError(h.protocol.name), Diagnostics: h.protocol.name + " returned a different native session identity"}
	}
	return HeadlessSession{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, Role: request.Role, NativeSessionID: nativeID}, nil
}

// cancelAfterLaunchFailure closes a helper that reached the detached worker
// but failed before returning a usable native identity. It uses a fresh cleanup
// context so a discovery timeout cannot leave the native process mutating the
// worktree after the coordinator has rejected the launch.
func (h *headless) cancelAfterLaunchFailure(ctx context.Context, request HeadlessStartRequest, nativeSessionID string) {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), headlessCleanupTimeout)
	defer cancel()
	_ = h.CancelHeadless(cleanupContext, HeadlessSession{
		InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID,
		Role: request.Role, NativeSessionID: nativeSessionID,
	})
}

// waitForNativeSession polls worker-owned state until the harness emits the
// machine event carrying its native session identity or a typed process
// outcome becomes observable.
func (h *headless) waitForNativeSession(ctx context.Context, request HeadlessStartRequest) (string, error) {
	inspectionContext, cancel := context.WithTimeout(ctx, headlessDiscoveryTimeout)
	defer cancel()
	delay := headlessDiscoveryInitialDelay
	for {
		inspection, err := h.InspectHeadless(inspectionContext, HeadlessInspectionRequest{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, Role: request.Role})
		if err != nil {
			if inspectionContext.Err() != nil {
				return "", normalizeHeadlessError(h.protocol.name, inspectionContext.Err())
			}
			return "", normalizeHeadlessError(h.protocol.name, err)
		}
		if nativeID := h.protocol.nativeSessionID(inspection.Stdout); nativeID != "" {
			return nativeID, nil
		}
		settled := inspection.Status != worker.HeadlessStatusRunning && inspection.Status != worker.HeadlessStatusStarting
		if failure := h.classifyInspection(inspection); failure != nil && settled {
			failure.Diagnostics = BoundedHeadlessDiagnostics(inspection)
			return "", failure
		}
		switch inspection.Status {
		case worker.HeadlessStatusExited, worker.HeadlessStatusLost:
			return "", &HeadlessFailure{Cause: NewUnexpectedExitError(h.protocol.name), Diagnostics: BoundedHeadlessDiagnostics(inspection)}
		case worker.HeadlessStatusCancelled:
			return "", &HeadlessFailure{Cause: context.Canceled, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
		case worker.HeadlessStatusMissing:
			// docker exec -d returns before the helper has created its state
			// directory. Keep polling through that expected startup race.
		}
		timer := time.NewTimer(delay)
		select {
		case <-inspectionContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", &HeadlessFailure{Cause: NewUnexpectedExitError(h.protocol.name), Diagnostics: BoundedHeadlessDiagnostics(inspection)}
		case <-timer.C:
			if delay < headlessDiscoveryMaximumDelay {
				delay *= 2
				if delay > headlessDiscoveryMaximumDelay {
					delay = headlessDiscoveryMaximumDelay
				}
			}
		}
	}
}

// classifyInspection classifies structured events from both retained process
// streams. A harness normally emits its machine events on stdout, but an
// adapter-owned stderr event must remain visible when stdout is empty or
// truncated.
func (h *headless) classifyInspection(inspection HeadlessInspection) *HeadlessFailure {
	output := inspection.Stdout
	if strings.TrimSpace(inspection.Stderr) != "" {
		output += "\n" + inspection.Stderr
	}
	return h.protocol.classify(output)
}

// expectHarness refuses an inspection addressed to a different adapter, so a
// native session is never read through the harness that did not create it.
func (h *headless) expectHarness(name string) error {
	if name != "" && name != h.protocol.name {
		return fmt.Errorf("%s headless adapter cannot inspect harness %q", h.protocol.name, name)
	}
	return nil
}

// workerRequestForSession rebuilds only the logical worker identity for
// idempotent cancellation and finish after durable replay.
func (h *headless) workerRequestForSession(session HeadlessSession) worker.HeadlessRequest {
	return worker.HeadlessRequest{RunID: session.RunID, WorkerID: session.WorkerID, InvocationID: session.InvocationID, EnvironmentPolicy: worker.EnvironmentPolicyClean, Mode: worker.HeadlessLaunchFresh}
}

// forEachHeadlessEvent visits every complete machine-readable line of bounded
// process output and stops early when the visitor returns false.
func forEachHeadlessEvent(output string, visit func(line string) bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxHeadlessEventLineBytes)
	for scanner.Scan() {
		if !visit(scanner.Text()) {
			return
		}
	}
}

// normalizeHeadlessError maps worker and process-boundary failures to the
// existing redacted coordinator vocabulary without classifying human prose.
func normalizeHeadlessError(harnessName string, err error) error {
	if err == nil {
		return nil
	}
	var failure *HeadlessFailure
	if errors.As(err, &failure) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &HeadlessFailure{Cause: context.Canceled}
	case errors.Is(err, ErrRateLimited):
		return &HeadlessFailure{Cause: NewRateLimitError(harnessName)}
	case errors.Is(err, ErrAuthenticationExpired):
		return &HeadlessFailure{Cause: NewAuthenticationExpiredError(harnessName)}
	default:
		return &HeadlessFailure{Cause: NewUnexpectedExitError(harnessName)}
	}
}

// HeadlessFailure carries local bounded diagnostics separately from the typed
// coordinator cause, keeping workflow-visible errors free of native output.
type HeadlessFailure struct {
	// Cause is a typed failure or context cancellation.
	Cause error
	// ExitCode is the detached process exit code when the process reached a
	// terminal state. It is retained for local diagnosis, not Error().
	ExitCode int
	// Diagnostics is bounded JSONL/stderr retained for local troubleshooting.
	Diagnostics string
}

// Error returns only the safe typed cause.
func (e *HeadlessFailure) Error() string {
	if e == nil || e.Cause == nil {
		return "headless harness failure"
	}
	return e.Cause.Error()
}

// Unwrap exposes the typed coordinator cause without diagnostics.
func (e *HeadlessFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// HeadlessDiagnostics returns local-only bounded process diagnostics.
func HeadlessDiagnostics(err error) string {
	var failure *HeadlessFailure
	if !errors.As(err, &failure) {
		return ""
	}
	return failure.Diagnostics
}

// validateHeadlessStartRequest validates the coordinator-owned launch fields.
// The harness name only labels a refusal; every adapter enforces the same
// neutral contract.
func validateHeadlessStartRequest(harnessName string, request HeadlessStartRequest) error {
	for field, value := range map[string]string{
		"invocation id": request.InvocationID, "run id": request.RunID,
		"worker id": request.WorkerID, "role": request.Role,
		"stage": request.Stage, "prompt": request.Prompt,
	} {
		if field == "worker id" && strings.TrimSpace(value) == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s headless %s is required", harnessName, field)
		}
		if field != "prompt" && strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s headless %s must be a single line", harnessName, field)
		}
	}
	if len(request.Prompt) > maxPromptBytes {
		return &PromptTooLargeError{Harness: harnessName, Bytes: len(request.Prompt), Limit: maxPromptBytes}
	}
	if request.ResumeSessionID != "" && !safeHeadlessIdentifier(request.ResumeSessionID) {
		return fmt.Errorf("%s headless resume session id is unsafe", harnessName)
	}
	if request.WorkerID != "" && !safeHeadlessIdentifier(request.WorkerID) {
		return fmt.Errorf("%s headless worker id is unsafe", harnessName)
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return fmt.Errorf("%s headless model contains unsafe characters", harnessName)
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return fmt.Errorf("%s headless reasoning effort contains unsafe characters", harnessName)
	}
	return nil
}

// validNativeSessionID accepts UUID-shaped Codex thread identities without
// binding the adapter to one particular UUID version.
func validNativeSessionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

// safeHeadlessIdentifier validates a nonempty logical identifier.
func safeHeadlessIdentifier(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.') {
			return false
		}
	}
	return true
}

// BoundedHeadlessDiagnostics joins the retained native streams for local
// capture and enforces the coordinator's diagnostic byte bound.
func BoundedHeadlessDiagnostics(inspection HeadlessInspection) string {
	var builder strings.Builder
	appendDiagnostic := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if builder.Len() != 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(label)
		builder.WriteString(": ")
		builder.WriteString(value)
	}
	appendDiagnostic("stdout", inspection.Stdout)
	appendDiagnostic("stderr", inspection.Stderr)
	diagnostic := builder.String()
	if len(diagnostic) <= maxHeadlessDiagnosticBytes {
		return diagnostic
	}
	marker := "\n[truncated]\n"
	retained := maxHeadlessDiagnosticBytes - len(marker)
	if retained <= 0 {
		return marker[:maxHeadlessDiagnosticBytes]
	}
	head := retained / 2
	tail := retained - head
	return diagnostic[:head] + marker + diagnostic[len(diagnostic)-tail:]
}

// containsAny reports whether value contains one of the machine event code
// markers supplied by the adapter protocol.
func containsAny(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
