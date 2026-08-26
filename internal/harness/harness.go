// Package harness contains role-specific interactive harness adapters. An
// adapter translates one harness-neutral invocation into the native commands
// of a configured coding tool. It owns no workflow, Git, retry, or
// terminal-layout decision; the coordinator owns all of those.
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

const (
	// NameCodex identifies the Codex adapter.
	NameCodex = "codex"
	// NameClaude identifies the Claude Code adapter.
	NameClaude = "claude"
)

// StartRequest contains the coordinator-owned identity and prompt for one
// visible harness invocation.
type StartRequest struct {
	// InvocationID identifies the exact report-producing invocation.
	InvocationID string
	// RunID identifies the worker run.
	RunID string
	// Role identifies the workflow role.
	Role string
	// Stage identifies the workflow stage.
	Stage string
	// CheckpointSHA binds a review invocation to the exact commit under review.
	// It is optional for non-review roles.
	CheckpointSHA string
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
	// NativeSessionID is the harness-native continuation identity. Codex
	// persists it and the adapter discovers it; Claude Code accepts an
	// adapter-assigned identifier at launch.
	NativeSessionID string
	// Surface is the opaque terminal surface carrying the session.
	Surface terminal.Surface
}

// Capabilities describes what a harness adapter can do, so the coordinator can
// decide dispatch without naming a specific tool in workflow code.
type Capabilities struct {
	// Name is the stable harness identity recorded on an invocation.
	Name string
	// NativeResume reports whether the adapter can continue an existing native
	// session rather than only starting a fresh one.
	NativeResume bool
}

// Runtime is the portable harness lifecycle seam used by the coordinator.
type Runtime interface {
	// Capabilities reports the adapter identity and supported lifecycle.
	Capabilities() Capabilities
	// Start launches a fresh interactive harness session.
	Start(context.Context, StartRequest) (Session, error)
	// Resume launches a native-resume session.
	Resume(context.Context, StartRequest) (Session, error)
	// Finish detaches the visible surface after accepted completion.
	Finish(context.Context, Session) error
}

// ErrUnsupportedSetting reports that a validated repository setting has no
// native representation in the selected harness. The adapter refuses the
// launch rather than silently dropping a policy selection.
var ErrUnsupportedSetting = errors.New("harness does not support the requested setting")

// ErrUnknownHarness reports that no adapter implements the requested harness.
var ErrUnknownHarness = errors.New("no adapter implements the requested harness")

// New creates the adapter for one validated harness name. It is the only place
// that maps a repository-declared harness onto an implementation.
func New(name string, workerRuntime worker.WorkerRuntime, terminalRuntime terminal.TerminalRuntime) (Runtime, error) {
	switch name {
	case NameCodex:
		return NewCodex(workerRuntime, terminalRuntime), nil
	case NameClaude:
		return NewClaude(workerRuntime, terminalRuntime), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
}

const (
	// nativeSessionDiscoveryTimeout bounds how long a fresh launch waits for a
	// harness to persist its native session file.
	nativeSessionDiscoveryTimeout = 60 * time.Second
	// nativeSessionDiscoveryInitialInterval avoids a tight polling loop while
	// the harness initializes its role home.
	nativeSessionDiscoveryInitialInterval = 100 * time.Millisecond
	// nativeSessionDiscoveryMaxInterval keeps discovery responsive without
	// repeatedly invoking the worker while the harness is still starting.
	nativeSessionDiscoveryMaxInterval = 500 * time.Millisecond
)

// ErrNativeSessionUnavailable reports that a fresh launch did not expose a
// newly persisted native session before discovery timed out or was canceled.
var ErrNativeSessionUnavailable = errors.New("native Codex session was not discovered before the deadline")

// discoverNativeSession returns a session identifier that was not present
// before the prompt-bearing command was launched. It returns
// ErrNativeSessionUnavailable when the harness does not persist a new session
// before the bounded discovery window expires or the caller cancels discovery.
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
// worker side effect occurs. The harness name only labels the refusal; every
// adapter enforces the same neutral contract.
func validateStartRequest(harnessName string, request StartRequest) error {
	for field, value := range map[string]string{
		"invocation id": request.InvocationID,
		"run id":        request.RunID,
		"role":          request.Role,
		"stage":         request.Stage,
		"prompt":        request.Prompt,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s %s is required", harnessName, field)
		}
		if strings.ContainsAny(value, "\x00\r\n") && field != "prompt" {
			return fmt.Errorf("%s %s must be a single line", harnessName, field)
		}
	}
	if request.WorkspaceID == "" {
		return fmt.Errorf("%s workspace id is required", harnessName)
	}
	if request.Model != "" && strings.ContainsAny(request.Model, "\x00\r\n ") {
		return fmt.Errorf("%s model contains unsafe characters", harnessName)
	}
	if request.ReasoningEffort != "" && strings.ContainsAny(request.ReasoningEffort, "\x00\r\n ") {
		return fmt.Errorf("%s reasoning effort contains unsafe characters", harnessName)
	}
	return nil
}

// invocationEnvironment builds the explicit non-secret identity values that a
// visible harness receives. It contains no credential, host path, or GitHub
// token, and every name is an accepted interactive protocol value.
func invocationEnvironment(harnessName string, request StartRequest) map[string]string {
	environment := map[string]string{
		"FACTORY_HARNESS":       harnessName,
		"FACTORY_INVOCATION_ID": request.InvocationID,
		"FACTORY_STAGE":         request.Stage,
	}
	if request.CheckpointSHA != "" {
		environment["FACTORY_CHECKPOINT_SHA"] = request.CheckpointSHA
	}
	if request.Model != "" {
		environment["FACTORY_MODEL"] = request.Model
	}
	return environment
}

// launchSurface reuses the coordinator-owned role surface when one exists and
// otherwise creates a role-named surface for the attach command. Layout choice
// stays with the coordinator; the adapter only places its own command.
func launchSurface(ctx context.Context, terminalRuntime terminal.TerminalRuntime, harnessName string, request StartRequest, attach worker.InteractiveCommand) (terminal.Surface, error) {
	command := terminal.Command{Executable: attach.Executable, Args: append([]string(nil), attach.Args...)}
	if request.Surface.ID == "" {
		surface, err := terminalRuntime.CreateSurface(ctx, terminal.SurfaceRequest{
			WorkspaceID: request.WorkspaceID,
			Name:        request.Role,
			Command:     command,
		})
		if err != nil {
			return terminal.Surface{}, fmt.Errorf("create %s surface: %w", harnessName, err)
		}
		return surface, nil
	}
	if err := terminalRuntime.LaunchSurface(ctx, request.Surface.ID, command); err != nil {
		return terminal.Surface{}, fmt.Errorf("launch %s surface: %w", harnessName, err)
	}
	return request.Surface, nil
}

// requestExit sends the harness's graceful exit command while leaving the
// opaque surface open, so cmux can recover it and a later coordinator can
// reattach or resume.
func requestExit(ctx context.Context, terminalRuntime terminal.TerminalRuntime, harnessName string, session Session) error {
	if terminalRuntime == nil {
		return fmt.Errorf("%s terminal runtime is required", harnessName)
	}
	if session.Surface.ID == "" {
		return fmt.Errorf("%s session surface is required", harnessName)
	}
	if err := terminalRuntime.SendInput(ctx, session.Surface.ID, []byte("/exit\n")); err != nil {
		return fmt.Errorf("request %s session exit: %w", harnessName, err)
	}
	return nil
}
