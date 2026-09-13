// Package harness contains headless coding-tool adapters. Adapters
// translate a harness-neutral invocation into a detached worker process and
// expose only durable native-session state to the coordinator.
package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	// NameCodex identifies the Codex adapter.
	NameCodex = "codex"
	// NameClaude identifies the Claude Code adapter.
	NameClaude = "claude"
	// maxPromptBytes leaves headroom beneath Linux's single-argument limit.
	maxPromptBytes = 96 << 10
)

// StartRequest contains coordinator-owned identity, prompt, and policy for one
// detached harness invocation.
type StartRequest struct {
	// InvocationID is the factory-assigned identity for this attempt.
	InvocationID string
	// RunID binds the invocation to its durable workflow run.
	RunID string
	// WorkerID selects the isolated worker process boundary.
	WorkerID string
	// Role and Stage are the factory-owned workflow selection.
	Role  string
	Stage string
	// CheckpointSHA is the immutable Git state supplied to the role.
	CheckpointSHA string
	// Prompt is the bounded factory-owned instruction envelope.
	Prompt string
	// Model and ReasoningEffort are validated repository policy selections.
	Model           string
	ReasoningEffort string
	// ResumeSessionID requests an exact native continuation when nonempty.
	ResumeSessionID string
}

// Session is the durable identity returned by a detached harness process.
type Session struct {
	// InvocationID and RunID echo the coordinator identities.
	InvocationID string
	RunID        string
	// WorkerID identifies the detached worker process boundary.
	WorkerID string
	// NativeSessionID is the harness-owned continuation identity.
	NativeSessionID string
}

// Capabilities describes one supported harness adapter.
type Capabilities struct {
	// Name is the stable configured harness identifier.
	Name string
	// NativeResume reports exact native-session continuation support.
	NativeResume bool
	// Headless reports detached execution without coordinator stream attachment.
	Headless bool
}

// NativeSessionRequest identifies one worker-backed native session.
type NativeSessionRequest struct {
	// RunID and InvocationID identify the durable factory attempt.
	RunID        string
	InvocationID string
	// WorkerID identifies the detached worker process to inspect.
	WorkerID string
	// Harness selects the adapter-specific native identity protocol.
	Harness string
}

// NativeSessionInspector observes the native identity owned by a process.
type NativeSessionInspector interface {
	// NativeSessionID returns the native identity observed for the process.
	NativeSessionID(context.Context, NativeSessionRequest) (string, error)
}

// NativeSessionLivenessInspector observes whether a native process is active.
type NativeSessionLivenessInspector interface {
	// NativeSessionRunning reports whether the native process is still active.
	NativeSessionRunning(context.Context, NativeSessionRequest) (bool, error)
}

// HeadlessFailureInspector classifies a settled detached process.
type HeadlessFailureInspector interface {
	// HeadlessFailureFor returns the classified settled-process failure, if any.
	HeadlessFailureFor(context.Context, HeadlessInspectionRequest) error
}

// Runtime is the detached harness lifecycle seam used by the coordinator and
// durable effect journal.
type Runtime interface {
	// Capabilities identifies the adapter and supported lifecycle operations.
	Capabilities() Capabilities
	// Start launches one new native harness session.
	Start(context.Context, StartRequest) (Session, error)
	// Resume continues exactly the supplied native session.
	Resume(context.Context, StartRequest) (Session, error)
	// Finish releases adapter-owned state after report acceptance.
	Finish(context.Context, Session) error
}

// NewHeadlessAdapters creates every supported headless adapter.
func NewHeadlessAdapters(processRuntime worker.HeadlessProcessRuntime) map[config.Harness]HeadlessRuntime {
	return map[config.Harness]HeadlessRuntime{
		config.HarnessCodex:  NewCodexHeadless(processRuntime),
		config.HarnessClaude: NewClaudeHeadless(processRuntime),
	}
}

// ErrUnknownHarness reports an unsupported repository harness selection.
var ErrUnknownHarness = errors.New("no adapter implements the requested harness")

// New creates a headless adapter for a validated harness name.
func New(name string, processRuntime worker.HeadlessProcessRuntime) (Runtime, error) {
	adapters := NewHeadlessAdapters(processRuntime)
	adapter, ok := adapters[config.Harness(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
	return AdaptHeadlessRuntime(adapter), nil
}

// invocationEnvironment builds the explicit non-secret invocation identity.
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
