package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	headlessDiscoveryTimeout      = 60 * time.Second
	headlessDiscoveryInitialDelay = 100 * time.Millisecond
	headlessDiscoveryMaximumDelay = 500 * time.Millisecond
	maxHeadlessDiagnosticBytes    = 16 << 10
	maxHeadlessEventLineBytes     = 1 << 20
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
	// Prompt is the frozen role prompt.
	Prompt string
	// Model is the repository-selected model.
	Model string
	// ReasoningEffort is the repository-selected reasoning policy.
	ReasoningEffort string
	// ResumeSessionID is the exact native Codex thread to continue.
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
	// NativeSessionID is the Codex thread identity read from a JSON event.
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
	// Stdout is bounded Codex JSONL output.
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
	// Capabilities reports the headless adapter identity and resume support.
	Capabilities() Capabilities
	// StartHeadless launches a fresh native process.
	StartHeadless(context.Context, HeadlessStartRequest) (HeadlessSession, error)
	// ResumeHeadless launches the exact recorded native session.
	ResumeHeadless(context.Context, HeadlessStartRequest) (HeadlessSession, error)
	// InspectHeadless reads process state and bounded machine output.
	InspectHeadless(context.Context, HeadlessInspectionRequest) (HeadlessInspection, error)
	// CancelHeadless requests idempotent process cancellation.
	CancelHeadless(context.Context, HeadlessSession) error
	// FinishHeadless performs idempotent accepted-completion shutdown.
	FinishHeadless(context.Context, HeadlessSession) error
}

// CodexHeadless implements HeadlessRuntime through the worker's detached
// process extension. No Codex command is ever started on the coordinator host.
type CodexHeadless struct {
	// Worker owns Docker translation, process state, and bounded capture.
	Worker worker.HeadlessProcessRuntime
}

// NewCodexHeadless creates a terminal-free Codex adapter.
func NewCodexHeadless(runtime worker.HeadlessProcessRuntime) *CodexHeadless {
	return &CodexHeadless{Worker: runtime}
}

// Capabilities reports Codex's headless native-resume support.
func (*CodexHeadless) Capabilities() Capabilities {
	return codexCapabilities(true)
}

// StartHeadless launches a fresh Codex exec process and waits for its machine
// readable thread-start event before returning the native identity.
func (c *CodexHeadless) StartHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) != "" {
		return HeadlessSession{}, errors.New("fresh Codex headless start cannot include a resume session")
	}
	return c.launch(ctx, request, worker.HeadlessLaunchFresh)
}

// ResumeHeadless launches Codex exec resume for the exact persisted native
// thread identity and never substitutes a newly generated session.
func (c *CodexHeadless) ResumeHeadless(ctx context.Context, request HeadlessStartRequest) (HeadlessSession, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return HeadlessSession{}, errors.New("Codex headless resume session id is required")
	}
	return c.launch(ctx, request, worker.HeadlessLaunchResume)
}

// InspectHeadless reads the worker-owned detached process projection.
func (c *CodexHeadless) InspectHeadless(ctx context.Context, request HeadlessInspectionRequest) (HeadlessInspection, error) {
	if c.Worker == nil {
		return HeadlessInspection{}, errors.New("worker runtime does not support headless processes")
	}
	result, err := c.Worker.InspectHeadless(ctx, worker.HeadlessRequest{
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
func (c *CodexHeadless) CancelHeadless(ctx context.Context, session HeadlessSession) error {
	if c.Worker == nil {
		return errors.New("worker runtime does not support headless processes")
	}
	return c.Worker.CancelHeadless(ctx, c.workerRequestForSession(session))
}

// FinishHeadless delegates accepted-completion shutdown to the worker and is
// safe to replay after a response-loss boundary.
func (c *CodexHeadless) FinishHeadless(ctx context.Context, session HeadlessSession) error {
	if c.Worker == nil {
		return errors.New("worker runtime does not support headless processes")
	}
	return c.Worker.FinishHeadless(ctx, c.workerRequestForSession(session))
}

// NativeSessionID returns the Codex thread identity recorded in the durable
// detached-process output. The worker remains the only component that reads
// the process state or role home, and the process-associated event prevents a
// coordinator restart from accidentally adopting an older role-home session.
func (c *CodexHeadless) NativeSessionID(ctx context.Context, request NativeSessionRequest) (string, error) {
	if request.Harness != "" && request.Harness != NameCodex {
		return "", fmt.Errorf("Codex headless adapter cannot inspect harness %q", request.Harness)
	}
	inspection, err := c.InspectHeadless(ctx, HeadlessInspectionRequest{
		InvocationID: request.InvocationID,
		RunID:        request.RunID,
		WorkerID:     request.WorkerID,
	})
	if err != nil {
		return "", err
	}
	if nativeID := threadStartedID(inspection.Stdout); nativeID != "" {
		return nativeID, nil
	}
	return "", errors.New("Codex headless process has no thread.started identity")
}

// NativeSessionRunning maps the durable detached process state to the
// coordinator's liveness observation without inspecting terminal output.
func (c *CodexHeadless) NativeSessionRunning(ctx context.Context, request NativeSessionRequest) (bool, error) {
	if request.Harness != "" && request.Harness != NameCodex {
		return false, fmt.Errorf("Codex headless adapter cannot inspect harness %q", request.Harness)
	}
	inspection, err := c.InspectHeadless(ctx, HeadlessInspectionRequest{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID})
	if err != nil {
		return false, err
	}
	switch inspection.Status {
	case worker.HeadlessStatusStarting, worker.HeadlessStatusRunning:
		return true, nil
	case worker.HeadlessStatusMissing, worker.HeadlessStatusExited, worker.HeadlessStatusCancelled, worker.HeadlessStatusLost:
		return false, nil
	default:
		return false, fmt.Errorf("unknown Codex headless process state %q", inspection.Status)
	}
}

// HeadlessFailureFor classifies a terminal detached-process inspection after
// the native thread identity is known. It closes the post-launch gap where a
// turn.failed event arrives after the launch polling window.
func (c *CodexHeadless) HeadlessFailureFor(ctx context.Context, request HeadlessInspectionRequest) error {
	inspection, err := c.InspectHeadless(ctx, request)
	if err != nil {
		return normalizeHeadlessError(err)
	}
	if inspection.Status == worker.HeadlessStatusMissing || inspection.Status == worker.HeadlessStatusStarting || inspection.Status == worker.HeadlessStatusRunning {
		return nil
	}
	if failure := classifyHeadlessInspection(inspection); failure != nil {
		failure.Diagnostics = BoundedHeadlessDiagnostics(inspection)
		return failure
	}
	switch inspection.Status {
	case worker.HeadlessStatusCancelled:
		return &HeadlessFailure{Cause: context.Canceled, ExitCode: inspection.ExitCode, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
	case worker.HeadlessStatusExited, worker.HeadlessStatusLost:
		// Preserve the process code for local diagnostics without putting it in
		// the redacted coordinator-visible error category.
		return &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex), ExitCode: inspection.ExitCode, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
	default:
		return fmt.Errorf("unknown Codex headless process state %q", inspection.Status)
	}
}

// launch translates one neutral request into the documented Codex exec JSONL
// command and waits for the first machine-readable thread identity.
func (c *CodexHeadless) launch(ctx context.Context, request HeadlessStartRequest, mode worker.HeadlessLaunchMode) (HeadlessSession, error) {
	if err := validateHeadlessStartRequest(request); err != nil {
		return HeadlessSession{}, err
	}
	if c.Worker == nil {
		return HeadlessSession{}, errors.New("worker runtime does not support headless processes")
	}
	command := codexCommandOptions([]string{"codex", "exec", "--json"}, request.Model, request.ReasoningEffort)
	if request.ResumeSessionID != "" {
		command = append(command, "resume", request.ResumeSessionID)
	}
	command = append(command, strings.TrimSpace(request.Prompt))
	environment := invocationEnvironment(NameCodex, StartRequest{
		InvocationID: request.InvocationID, RunID: request.RunID, Role: request.Role,
		Stage: request.Stage, CheckpointSHA: request.CheckpointSHA, Model: request.Model,
	})
	if request.ReasoningEffort != "" {
		environment["FACTORY_REASONING_EFFORT"] = request.ReasoningEffort
	}
	_, err := c.Worker.StartHeadless(ctx, worker.HeadlessRequest{
		RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID,
		Command: command, EnvironmentPolicy: worker.EnvironmentPolicyRole, Role: request.Role,
		Environment: environment, Mode: mode,
	})
	if err != nil {
		return HeadlessSession{}, normalizeHeadlessError(err)
	}
	_, nativeID, err := c.waitForThreadStarted(ctx, request)
	if err != nil {
		c.cancelAfterLaunchFailure(ctx, request, nativeID)
		return HeadlessSession{}, err
	}
	if mode == worker.HeadlessLaunchResume && nativeID != request.ResumeSessionID {
		c.cancelAfterLaunchFailure(ctx, request, nativeID)
		return HeadlessSession{}, &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex), Diagnostics: "Codex resume returned a different native thread identity"}
	}
	return HeadlessSession{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, Role: request.Role, NativeSessionID: nativeID}, nil
}

// cancelAfterLaunchFailure closes a helper that reached the detached worker
// but failed before returning a usable native identity. It uses a fresh cleanup
// context so a discovery timeout cannot leave the native process mutating the
// worktree after the coordinator has rejected the launch.
func (c *CodexHeadless) cancelAfterLaunchFailure(ctx context.Context, request HeadlessStartRequest, nativeSessionID string) {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = c.CancelHeadless(cleanupContext, HeadlessSession{
		InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID,
		Role: request.Role, NativeSessionID: nativeSessionID,
	})
}

// waitForThreadStarted polls worker-owned state until Codex emits its JSONL
// thread.started event or a typed process outcome becomes observable.
func (c *CodexHeadless) waitForThreadStarted(ctx context.Context, request HeadlessStartRequest) (HeadlessInspection, string, error) {
	inspectionContext, cancel := context.WithTimeout(ctx, headlessDiscoveryTimeout)
	defer cancel()
	delay := headlessDiscoveryInitialDelay
	for {
		inspection, err := c.InspectHeadless(inspectionContext, HeadlessInspectionRequest{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, Role: request.Role})
		if err != nil {
			if inspectionContext.Err() != nil {
				return HeadlessInspection{}, "", normalizeHeadlessError(inspectionContext.Err())
			}
			return HeadlessInspection{}, "", normalizeHeadlessError(err)
		}
		if nativeID := threadStartedID(inspection.Stdout); nativeID != "" {
			return inspection, nativeID, nil
		}
		if failure := classifyHeadlessInspection(inspection); failure != nil && inspection.Status != worker.HeadlessStatusRunning && inspection.Status != worker.HeadlessStatusStarting {
			failure.Diagnostics = BoundedHeadlessDiagnostics(inspection)
			return inspection, "", failure
		}
		switch inspection.Status {
		case worker.HeadlessStatusExited, worker.HeadlessStatusLost:
			failure := &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex), Diagnostics: BoundedHeadlessDiagnostics(inspection)}
			return inspection, "", failure
		case worker.HeadlessStatusCancelled:
			return inspection, "", &HeadlessFailure{Cause: context.Canceled, Diagnostics: BoundedHeadlessDiagnostics(inspection)}
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
			return inspection, "", &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex), Diagnostics: BoundedHeadlessDiagnostics(inspection)}
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

// classifyHeadlessInspection classifies structured events from both retained
// process streams. Codex normally emits JSONL on stdout, but an adapter-owned
// stderr event must remain visible when stdout is empty or truncated.
func classifyHeadlessInspection(inspection HeadlessInspection) *HeadlessFailure {
	output := inspection.Stdout
	if strings.TrimSpace(inspection.Stderr) != "" {
		output += "\n" + inspection.Stderr
	}
	return classifyHeadlessEvents(output)
}

// threadStartedID extracts only a valid thread.started machine event and never
// treats human-readable output as a native session identity.
func threadStartedID(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxHeadlessEventLineBytes)
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(scanner.Text()), &event) != nil || event.Type != "thread.started" || !validNativeSessionID(event.ThreadID) {
			continue
		}
		return event.ThreadID
	}
	return ""
}

// classifyHeadlessEvents maps structured Codex failure events to existing
// typed coordinator outcomes. Human-readable text is considered only when it
// is carried by a documented failure event, never when it is standalone prose.
func classifyHeadlessEvents(output string) *HeadlessFailure {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 4096), maxHeadlessEventLineBytes)
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Error   struct {
				Code       string `json:"code"`
				Type       string `json:"type"`
				Message    string `json:"message"`
				Status     int    `json:"status"`
				StatusCode int    `json:"status_code"`
				CodexError struct {
					Code    string `json:"code"`
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"codex_error_info"`
			} `json:"error"`
			Status     int `json:"status"`
			StatusCode int `json:"status_code"`
		}
		if json.Unmarshal([]byte(scanner.Text()), &event) != nil {
			continue
		}
		code := strings.ToLower(strings.TrimSpace(event.Code + " " + event.Error.Code + " " + event.Error.Type + " " + event.Error.CodexError.Code + " " + event.Error.CodexError.Type))
		message := strings.ToLower(strings.TrimSpace(event.Message + " " + event.Error.Message + " " + event.Error.CodexError.Message))
		status := event.Status
		if status == 0 {
			status = event.StatusCode
		}
		if status == 0 {
			status = event.Error.Status
		}
		if status == 0 {
			status = event.Error.StatusCode
		}
		if event.Type != "error" && event.Type != "turn.failed" && strings.TrimSpace(code) == "" && status == 0 {
			continue
		}
		failureText := code + " " + message
		if status != 0 {
			failureText += fmt.Sprintf(" status %d", status)
		}
		switch {
		case containsAny(failureText, "rate_limit", "rate-limit", "rate limit", "too_many_requests", "too many requests", "capacity", "quota", "status 429", "http 429"):
			return &HeadlessFailure{Cause: NewRateLimitError(NameCodex)}
		case containsAny(failureText, "unauthorized", "authentication", "auth_required", "invalid_api_key", "invalid api key", "credential", "token_expired", "token expired", "status 401", "http 401"):
			return &HeadlessFailure{Cause: NewAuthenticationExpiredError(NameCodex)}
		case containsAny(failureText, "cancel", "aborted"):
			return &HeadlessFailure{Cause: context.Canceled}
		case event.Type == "error" || event.Type == "turn.failed":
			return &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex)}
		}
	}
	return nil
}

// normalizeHeadlessError maps worker and process-boundary failures to the
// existing redacted coordinator vocabulary without classifying human prose.
func normalizeHeadlessError(err error) error {
	if err == nil {
		return nil
	}
	var failure *HeadlessFailure
	if errors.As(err, &failure) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return &HeadlessFailure{Cause: context.Canceled}
	}
	if errors.Is(err, ErrRateLimited) {
		return &HeadlessFailure{Cause: NewRateLimitError(NameCodex)}
	}
	if errors.Is(err, ErrAuthenticationExpired) {
		return &HeadlessFailure{Cause: NewAuthenticationExpiredError(NameCodex)}
	}
	if errors.Is(err, ErrUnexpectedExit) {
		return &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex)}
	}
	return &HeadlessFailure{Cause: NewUnexpectedExitError(NameCodex)}
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

// AdaptHeadlessRuntime supplies the legacy Runtime shape to durable effect
// replay while preserving the coordinator-owned terminal-free implementation.
func AdaptHeadlessRuntime(runtime HeadlessRuntime) Runtime {
	if runtime == nil {
		return nil
	}
	return &headlessRuntimeAdapter{runtime: runtime}
}

// headlessRuntimeAdapter is the narrow compatibility bridge used by existing
// effect journals while new lifecycle code resolves HeadlessRuntime directly.
type headlessRuntimeAdapter struct {
	runtime HeadlessRuntime
}

// Capabilities delegates static headless capabilities.
func (a *headlessRuntimeAdapter) Capabilities() Capabilities {
	capabilities := a.runtime.Capabilities()
	// A HeadlessRuntime has no terminal topology by construction. Keep the
	// compatibility capability true even for small embedding fakes that only
	// fill the adapter name and resume bit.
	capabilities.Headless = true
	return capabilities
}

// Start maps the legacy request and rejects any terminal topology.
func (a *headlessRuntimeAdapter) Start(ctx context.Context, request StartRequest) (Session, error) {
	if hasTerminalTopology(request.WorkspaceID, request.Surface) {
		return Session{}, errors.New("headless harness cannot receive terminal workspace or surface")
	}
	result, err := a.runtime.StartHeadless(ctx, headlessRequest(request))
	return sessionFromHeadless(result), err
}

// Resume maps the legacy request and rejects any terminal topology.
func (a *headlessRuntimeAdapter) Resume(ctx context.Context, request StartRequest) (Session, error) {
	if hasTerminalTopology(request.WorkspaceID, request.Surface) {
		return Session{}, errors.New("headless harness cannot receive terminal workspace or surface")
	}
	result, err := a.runtime.ResumeHeadless(ctx, headlessRequest(request))
	return sessionFromHeadless(result), err
}

// Finish maps legacy effect state to idempotent headless cleanup.
func (a *headlessRuntimeAdapter) Finish(ctx context.Context, session Session) error {
	if hasTerminalTopology("", session.Surface) {
		return errors.New("headless harness cannot finish a terminal session")
	}
	return a.runtime.FinishHeadless(ctx, HeadlessSession{InvocationID: session.InvocationID, RunID: session.RunID, WorkerID: session.WorkerID, NativeSessionID: session.NativeSessionID})
}

// hasTerminalTopology reports whether a legacy-shaped request carries any
// workspace or surface field that a headless adapter must reject.
func hasTerminalTopology(workspaceID terminal.WorkspaceID, surface terminal.Surface) bool {
	return workspaceID != "" || surface.ID != "" || surface.WorkspaceID != "" || strings.TrimSpace(surface.Name) != ""
}

// NativeSessionID forwards headless identity inspection through the legacy
// effect adapter without exposing worker or terminal implementation details.
func (a *headlessRuntimeAdapter) NativeSessionID(ctx context.Context, request NativeSessionRequest) (string, error) {
	inspector, ok := a.runtime.(NativeSessionInspector)
	if !ok {
		return "", errors.New("headless harness does not support native session inspection")
	}
	return inspector.NativeSessionID(ctx, request)
}

// NativeSessionRunning forwards detached-process liveness through the legacy
// effect adapter so restart diagnosis remains terminal-free.
func (a *headlessRuntimeAdapter) NativeSessionRunning(ctx context.Context, request NativeSessionRequest) (bool, error) {
	inspector, ok := a.runtime.(NativeSessionLivenessInspector)
	if !ok {
		return false, errors.New("headless harness does not support native session liveness")
	}
	return inspector.NativeSessionRunning(ctx, request)
}

// HeadlessFailureFor forwards terminal detached-process classification through
// the compatibility bridge used by journal replay and lifecycle monitoring.
func (a *headlessRuntimeAdapter) HeadlessFailureFor(ctx context.Context, request HeadlessInspectionRequest) error {
	inspector, ok := a.runtime.(HeadlessFailureInspector)
	if !ok {
		return nil
	}
	return inspector.HeadlessFailureFor(ctx, request)
}

// headlessRequest translates the legacy prompt contract to the headless seam.
func headlessRequest(request StartRequest) HeadlessStartRequest {
	return HeadlessStartRequest{InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID, Role: request.Role, Stage: request.Stage, CheckpointSHA: request.CheckpointSHA, Prompt: request.Prompt, Model: request.Model, ReasoningEffort: request.ReasoningEffort, ResumeSessionID: request.ResumeSessionID}
}

// sessionFromHeadless translates one headless identity into the effect seam.
func sessionFromHeadless(session HeadlessSession) Session {
	return Session{InvocationID: session.InvocationID, RunID: session.RunID, WorkerID: session.WorkerID, NativeSessionID: session.NativeSessionID}
}

// workerRequestForSession rebuilds only the logical worker identity for
// idempotent cancellation and finish after durable replay.
func (c *CodexHeadless) workerRequestForSession(session HeadlessSession) worker.HeadlessRequest {
	return worker.HeadlessRequest{RunID: session.RunID, WorkerID: session.WorkerID, InvocationID: session.InvocationID, EnvironmentPolicy: worker.EnvironmentPolicyClean, Mode: worker.HeadlessLaunchFresh}
}

// validateHeadlessStartRequest validates the coordinator-owned launch fields.
func validateHeadlessStartRequest(request HeadlessStartRequest) error {
	for field, value := range map[string]string{
		"invocation id": request.InvocationID, "run id": request.RunID,
		"worker id": request.WorkerID, "role": request.Role,
		"stage": request.Stage, "prompt": request.Prompt,
	} {
		if field == "worker id" && strings.TrimSpace(value) == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Codex headless %s is required", field)
		}
		if field != "prompt" && strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("Codex headless %s must be a single line", field)
		}
	}
	if len(request.Prompt) > maxPromptBytes {
		return &PromptTooLargeError{Harness: NameCodex, Bytes: len(request.Prompt), Limit: maxPromptBytes}
	}
	if request.WorkerID != "" && !safeHeadlessIdentifier(request.WorkerID) {
		return errors.New("Codex headless worker id is unsafe")
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return errors.New("Codex headless model contains unsafe characters")
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return errors.New("Codex headless reasoning effort contains unsafe characters")
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

var _ HeadlessRuntime = (*CodexHeadless)(nil)
var _ NativeSessionInspector = (*CodexHeadless)(nil)
var _ NativeSessionLivenessInspector = (*CodexHeadless)(nil)
var _ HeadlessFailureInspector = (*CodexHeadless)(nil)
var _ Runtime = (*headlessRuntimeAdapter)(nil)
