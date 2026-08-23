// Package harness contains role-specific interactive harness adapters.
package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// StartRequest contains the coordinator-owned identity and prompt for one
// visible Codex invocation.
type StartRequest struct {
	// InvocationID identifies the exact report-producing invocation.
	InvocationID string
	// RunID identifies the worker run.
	RunID string
	// Role identifies the workflow role.
	Role string
	// Stage identifies the workflow stage.
	Stage string
	// WorkspaceID identifies the terminal workspace receiving the surface.
	WorkspaceID terminal.WorkspaceID
	// Surface is an existing role surface in the run workspace. When empty, the
	// adapter creates a role-named surface.
	Surface terminal.Surface
	// Prompt is the initial role prompt passed to the harness process.
	Prompt string
	// Model is a validated repository-policy model selection.
	Model string
	// ReasoningEffort is a validated repository-policy setting.
	ReasoningEffort string
	// ResumeSessionID is set only when a native session is being resumed.
	ResumeSessionID string
}

// Session is the recoverable visible state returned after launch.
type Session struct {
	// InvocationID identifies the invocation owning the session.
	InvocationID string
	// NativeSessionID is known for resumes and may be populated by later
	// adapter discovery for a newly launched Codex session.
	NativeSessionID string
	// Surface is the opaque terminal surface carrying the session.
	Surface terminal.Surface
}

// Runtime is the portable harness lifecycle seam used by the coordinator.
type Runtime interface {
	// Start launches a fresh interactive harness session.
	Start(context.Context, StartRequest) (Session, error)
	// Resume launches a native-resume session.
	Resume(context.Context, StartRequest) (Session, error)
	// Finish detaches the visible surface after accepted completion.
	Finish(context.Context, Session) error
}

// Codex implements Runtime using WorkerRuntime's opaque interactive command
// extension and TerminalRuntime's visible surface operations.
type Codex struct {
	// Worker provides the worker-side interactive command translation.
	Worker worker.WorkerRuntime
	// Terminal owns the visible workspace and surface.
	Terminal terminal.TerminalRuntime
}

// NewCodex creates a Codex harness adapter.
func NewCodex(runtime worker.WorkerRuntime, terminalRuntime terminal.TerminalRuntime) *Codex {
	return &Codex{Worker: runtime, Terminal: terminalRuntime}
}

// Start launches a fresh Codex TUI in a worker-backed terminal surface.
func (c *Codex) Start(ctx context.Context, request StartRequest) (Session, error) {
	if request.ResumeSessionID != "" {
		return Session{}, errors.New("fresh Codex start cannot include a resume session")
	}
	return c.launch(ctx, request)
}

// Resume launches Codex with its native session identifier and preserves the
// required global security flags before the resume subcommand.
func (c *Codex) Resume(ctx context.Context, request StartRequest) (Session, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return Session{}, errors.New("Codex resume session id is required")
	}
	return c.launch(ctx, request)
}

// Finish requests a graceful Codex exit while leaving the opaque surface open
// so cmux can recover it and a later coordinator can reattach or resume.
func (c *Codex) Finish(ctx context.Context, session Session) error {
	if c.Terminal == nil {
		return errors.New("Codex terminal runtime is required")
	}
	if session.Surface.ID == "" {
		return errors.New("Codex session surface is required")
	}
	if err := c.Terminal.SendInput(ctx, session.Surface.ID, []byte("/exit\n")); err != nil {
		return fmt.Errorf("request Codex session exit: %w", err)
	}
	return nil
}

// launch builds the safe Codex command with its immutable prompt and creates a
// visible surface. It never uses terminal input or output as a launch protocol.
func (c *Codex) launch(ctx context.Context, request StartRequest) (Session, error) {
	if err := validateStartRequest(request); err != nil {
		return Session{}, err
	}
	interactive, ok := c.Worker.(worker.InteractiveRuntime)
	if !ok {
		return Session{}, errors.New("worker runtime does not support interactive harnesses")
	}
	if c.Terminal == nil {
		return Session{}, errors.New("Codex terminal runtime is required")
	}
	// The worker is the security boundary. Codex therefore runs without its
	// redundant inner sandbox or interactive approval gates, which would stall
	// an unattended invocation despite the worker's outer isolation.
	command := []string{"codex", "-a", "never", "-s", "danger-full-access"}
	if request.Model != "" {
		command = append(command, "-m", request.Model)
	}
	if request.ReasoningEffort != "" {
		command = append(command, "-c", "model_reasoning_effort="+request.ReasoningEffort)
	}
	if request.ResumeSessionID != "" {
		command = append(command, "resume", request.ResumeSessionID)
	}
	command = append(command, strings.TrimSpace(request.Prompt))
	environment := map[string]string{
		"FACTORY_HARNESS":       "codex",
		"FACTORY_INVOCATION_ID": request.InvocationID,
		"FACTORY_STAGE":         request.Stage,
	}
	if request.Model != "" {
		environment["FACTORY_MODEL"] = request.Model
	}
	if request.ReasoningEffort != "" {
		environment["FACTORY_REASONING_EFFORT"] = request.ReasoningEffort
	}
	attach, err := interactive.InteractiveCommand(ctx, worker.InteractiveRequest{
		RunID:             request.RunID,
		Command:           command,
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              request.Role,
		Environment:       environment,
	})
	if err != nil {
		return Session{}, fmt.Errorf("build Codex interactive command: %w", err)
	}
	var snapshotProvider worker.NativeSessionSnapshotProvider
	var baseline []string
	if request.ResumeSessionID == "" {
		if provider, ok := c.Worker.(worker.NativeSessionSnapshotProvider); ok {
			snapshotProvider = provider
			baseline, err = provider.NativeSessionIDs(ctx, worker.NativeSessionRequest{RunID: request.RunID, Harness: "codex"})
			if err != nil {
				return Session{}, fmt.Errorf("snapshot Codex native sessions: %w", err)
			}
		}
	}
	surface := request.Surface
	if surface.ID == "" {
		surface, err = c.Terminal.CreateSurface(ctx, terminal.SurfaceRequest{
			WorkspaceID: request.WorkspaceID,
			Name:        request.Role,
			Command: terminal.Command{
				Executable: attach.Executable,
				Args:       append([]string(nil), attach.Args...),
			},
		})
		if err != nil {
			return Session{}, fmt.Errorf("create Codex surface: %w", err)
		}
	} else if err := c.Terminal.LaunchSurface(ctx, surface.ID, terminal.Command{
		Executable: attach.Executable,
		Args:       append([]string(nil), attach.Args...),
	}); err != nil {
		return Session{}, fmt.Errorf("launch Codex surface: %w", err)
	}
	nativeSessionID := request.ResumeSessionID
	if nativeSessionID == "" {
		if snapshotProvider != nil {
			discovered, discoverErr := discoverNativeSession(ctx, snapshotProvider, baseline, worker.NativeSessionRequest{RunID: request.RunID, Harness: "codex"})
			if discoverErr != nil {
				_ = c.Terminal.CloseSurface(ctx, surface.ID)
				return Session{}, fmt.Errorf("discover Codex native session: %w", discoverErr)
			}
			nativeSessionID = discovered
		} else if provider, ok := c.Worker.(worker.NativeSessionProvider); ok {
			discovered, discoverErr := provider.NativeSessionID(ctx, worker.NativeSessionRequest{RunID: request.RunID, Harness: "codex"})
			if discoverErr != nil {
				_ = c.Terminal.CloseSurface(ctx, surface.ID)
				return Session{}, fmt.Errorf("discover Codex native session: %w", discoverErr)
			}
			nativeSessionID = discovered
		}
	}
	return Session{InvocationID: request.InvocationID, NativeSessionID: nativeSessionID, Surface: surface}, nil
}

const (
	// nativeSessionDiscoveryTimeout bounds how long a fresh launch waits for
	// Codex to persist its native session file.
	nativeSessionDiscoveryTimeout = 60 * time.Second
	// nativeSessionDiscoveryInitialInterval avoids a tight polling loop while
	// Codex initializes its role home.
	nativeSessionDiscoveryInitialInterval = 100 * time.Millisecond
	// nativeSessionDiscoveryMaxInterval keeps discovery responsive without
	// repeatedly invoking the worker while Codex is still starting.
	nativeSessionDiscoveryMaxInterval = 500 * time.Millisecond
)

// ErrNativeSessionUnavailable reports that a fresh Codex launch did not expose
// a newly persisted native session before discovery timed out or was canceled.
var ErrNativeSessionUnavailable = errors.New("native Codex session was not discovered before the deadline")

// discoverNativeSession returns a session identifier that was not present
// before the prompt-bearing command was launched. It returns
// ErrNativeSessionUnavailable when
// Codex does not persist a new session before the bounded discovery window
// expires or the caller cancels discovery.
func discoverNativeSession(ctx context.Context, provider worker.NativeSessionSnapshotProvider, baseline []string, request worker.NativeSessionRequest) (string, error) {
	discoveryContext, cancel := context.WithTimeout(ctx, nativeSessionDiscoveryTimeout)
	defer cancel()
	known := make(map[string]struct{}, len(baseline))
	for _, identifier := range baseline {
		known[identifier] = struct{}{}
	}
	interval := nativeSessionDiscoveryInitialInterval
	for {
		if err := discoveryContext.Err(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrNativeSessionUnavailable, err)
		}
		identifiers, err := provider.NativeSessionIDs(discoveryContext, request)
		if err != nil {
			if discoveryContext.Err() != nil {
				return "", fmt.Errorf("%w: %v", ErrNativeSessionUnavailable, discoveryContext.Err())
			}
			return "", err
		}
		for index := len(identifiers) - 1; index >= 0; index-- {
			if strings.TrimSpace(identifiers[index]) == "" {
				continue
			}
			if _, exists := known[identifiers[index]]; !exists {
				return identifiers[index], nil
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-discoveryContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", fmt.Errorf("%w: %v", ErrNativeSessionUnavailable, discoveryContext.Err())
		case <-timer.C:
			if interval < nativeSessionDiscoveryMaxInterval {
				interval *= 2
				if interval > nativeSessionDiscoveryMaxInterval {
					interval = nativeSessionDiscoveryMaxInterval
				}
			}
		}
	}
}

// validateStartRequest rejects incomplete identities before any terminal or
// worker side effect occurs.
func validateStartRequest(request StartRequest) error {
	for field, value := range map[string]string{
		"invocation id": request.InvocationID,
		"run id":        request.RunID,
		"role":          request.Role,
		"stage":         request.Stage,
		"prompt":        request.Prompt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Codex %s is required", field)
		}
		if strings.ContainsAny(value, "\x00\r\n") && field != "prompt" {
			return fmt.Errorf("Codex %s must be a single line", field)
		}
	}
	if request.WorkspaceID == "" {
		return errors.New("Codex workspace id is required")
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return errors.New("Codex model contains unsafe characters")
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return errors.New("Codex reasoning effort contains unsafe characters")
	}
	return nil
}

var _ Runtime = (*Codex)(nil)
