package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// emptyMCPConfiguration is the explicit empty MCP server set passed alongside
// the strict flag, so a launched session loads no server from any file.
const emptyMCPConfiguration = `{"mcpServers":{}}`

// Claude implements Runtime for Claude Code using the same portable worker and
// terminal seams as the Codex adapter.
type Claude struct {
	// Worker provides the worker-side interactive command translation.
	Worker worker.WorkerRuntime
	// Terminal owns the visible workspace and surface.
	Terminal terminal.TerminalRuntime
	// NewSessionID assigns the native session identifier for a fresh launch.
	// Claude Code accepts a caller-supplied identifier, so the adapter never
	// has to discover one after the process starts.
	NewSessionID func() (string, error)
}

// NewClaude creates a Claude Code harness adapter.
func NewClaude(runtime worker.WorkerRuntime, terminalRuntime terminal.TerminalRuntime) *Claude {
	return &Claude{Worker: runtime, Terminal: terminalRuntime, NewSessionID: newSessionID}
}

// Capabilities reports the Claude adapter identity and native resume support.
func (*Claude) Capabilities() Capabilities {
	return Capabilities{Name: NameClaude, NativeResume: true}
}

// Start launches a fresh Claude Code TUI with an adapter-assigned native
// session identifier in a worker-backed terminal surface.
func (c *Claude) Start(ctx context.Context, request StartRequest) (Session, error) {
	if request.ResumeSessionID != "" {
		return Session{}, errors.New("fresh Claude start cannot include a resume session")
	}
	generate := c.NewSessionID
	if generate == nil {
		generate = newSessionID
	}
	assigned, err := generate()
	if err != nil {
		return Session{}, fmt.Errorf("assign Claude native session id: %w", err)
	}
	return c.launch(ctx, request, assigned, false)
}

// Resume continues the recorded native Claude Code session and never assigns a
// second identifier for the same session.
func (c *Claude) Resume(ctx context.Context, request StartRequest) (Session, error) {
	if strings.TrimSpace(request.ResumeSessionID) == "" {
		return Session{}, errors.New("Claude resume session id is required")
	}
	return c.launch(ctx, request, request.ResumeSessionID, true)
}

// Finish requests a graceful Claude Code exit while leaving the opaque surface
// open so cmux can recover it and a later coordinator can reattach or resume.
func (c *Claude) Finish(ctx context.Context, session Session) error {
	return requestExit(ctx, c.Terminal, "Claude", session)
}

// launch builds the safe Claude Code command with its immutable prompt and
// places it in the coordinator-owned surface. It never uses terminal input or
// output as a launch protocol.
func (c *Claude) launch(ctx context.Context, request StartRequest, sessionID string, resume bool) (Session, error) {
	if err := validateStartRequest("Claude", request); err != nil {
		return Session{}, err
	}
	if request.ReasoningEffort != "" {
		// Claude Code exposes no reasoning-effort process argument. Dropping a
		// validated repository selection would run the role at an effort the
		// policy never authorized, so the launch is refused instead.
		return Session{}, fmt.Errorf("%w: Claude reasoning effort", ErrUnsupportedSetting)
	}
	if !validSessionID(sessionID) {
		return Session{}, errors.New("Claude native session id must be a version-four UUID")
	}
	interactive, ok := c.Worker.(worker.InteractiveRuntime)
	if !ok {
		return Session{}, errors.New("worker runtime does not support interactive harnesses")
	}
	if c.Terminal == nil {
		return Session{}, errors.New("Claude terminal runtime is required")
	}
	// The worker is the security boundary, so Claude Code runs without its
	// redundant interactive approval gates, which would stall an unattended
	// invocation. The strict MCP flags keep a factory session free of any
	// server declared by an ambient configuration file.
	command := []string{"claude", "--dangerously-skip-permissions", "--strict-mcp-config", "--mcp-config", emptyMCPConfiguration}
	if resume {
		command = append(command, "--resume", sessionID)
	} else {
		command = append(command, "--session-id", sessionID)
	}
	if request.Model != "" {
		command = append(command, "--model", request.Model)
	}
	command = append(command, strings.TrimSpace(request.Prompt))
	environment := invocationEnvironment(NameClaude, request)
	// The harness version is pinned by the worker image. Claude Code otherwise
	// attempts a self-update at startup, which must never change the tool
	// halfway through a run.
	environment["DISABLE_AUTOUPDATER"] = "1"
	attach, err := interactive.InteractiveCommand(ctx, worker.InteractiveRequest{
		RunID:             request.RunID,
		Command:           command,
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              request.Role,
		Environment:       environment,
	})
	if err != nil {
		return Session{}, fmt.Errorf("build Claude interactive command: %w", err)
	}
	surface, err := launchSurface(ctx, c.Terminal, "Claude", request, attach)
	if err != nil {
		return Session{}, err
	}
	return Session{InvocationID: request.InvocationID, NativeSessionID: sessionID, Surface: surface}, nil
}

// newSessionID returns a random RFC 4122 version-four UUID for one fresh
// Claude Code session.
func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// validSessionID reports whether value is a version-four UUID. A native
// session identifier becomes a process argument, so an unexpected shape is
// refused before any launch rather than passed through.
func validSessionID(value string) bool {
	if len(value) != 36 || value[14] != '4' {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			isDigit := character >= '0' && character <= '9'
			isHex := character >= 'a' && character <= 'f'
			if !isDigit && !isHex {
				return false
			}
		}
	}
	switch value[19] {
	case '8', '9', 'a', 'b':
		return true
	default:
		return false
	}
}

var _ Runtime = (*Claude)(nil)
