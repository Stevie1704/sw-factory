package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

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

// Capabilities reports the Codex adapter identity and native resume support.
func (*Codex) Capabilities() Capabilities {
	return Capabilities{Name: NameCodex, ReasoningEffort: true}
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
	return requestExit(ctx, c.Terminal, NameCodex, session)
}

// launch builds the safe Codex command with its immutable prompt and creates a
// visible surface. It never uses terminal input or output as a launch protocol.
func (c *Codex) launch(ctx context.Context, request StartRequest) (Session, error) {
	if err := validateStartRequest(NameCodex, request); err != nil {
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
	environment := invocationEnvironment(NameCodex, request)
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
			baseline, err = provider.NativeSessionIDs(ctx, worker.NativeSessionRequest{RunID: request.RunID, Harness: NameCodex})
			if err != nil {
				return Session{}, fmt.Errorf("snapshot Codex native sessions: %w", err)
			}
		}
	}
	surface, err := launchSurface(ctx, c.Terminal, NameCodex, request, attach)
	if err != nil {
		return Session{}, err
	}
	nativeSessionID := request.ResumeSessionID
	if nativeSessionID == "" {
		if snapshotProvider != nil {
			discovered, discoverErr := discoverNativeSession(ctx, snapshotProvider, baseline, worker.NativeSessionRequest{RunID: request.RunID, Harness: NameCodex})
			if discoverErr != nil {
				_ = c.Terminal.CloseSurface(ctx, surface.ID)
				return Session{}, fmt.Errorf("discover Codex native session: %w", discoverErr)
			}
			nativeSessionID = discovered
		} else if provider, ok := c.Worker.(worker.NativeSessionProvider); ok {
			discovered, discoverErr := provider.NativeSessionID(ctx, worker.NativeSessionRequest{RunID: request.RunID, Harness: NameCodex})
			if discoverErr != nil {
				_ = c.Terminal.CloseSurface(ctx, surface.ID)
				return Session{}, fmt.Errorf("discover Codex native session: %w", discoverErr)
			}
			nativeSessionID = discovered
		}
	}
	return Session{InvocationID: request.InvocationID, NativeSessionID: nativeSessionID, Surface: surface}, nil
}

var _ Runtime = (*Codex)(nil)
