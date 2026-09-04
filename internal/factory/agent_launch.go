package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// failedLaunchVoidMarker names the launch failure the coordinator may void:
// one that left neither a native session nor a structured report. It is the
// single wording every seam matches, so a wrapped error is never annotated
// with the same rule twice.
const failedLaunchVoidMarker = "harness launch produced no native session or structured report"

// failedLaunchHasNoEvidence reports whether a persisted launch has no native
// session or structured report that a later recovery must protect. An
// indeterminate report inspection fails closed so unreadable evidence remains
// protected.
func failedLaunchHasNoEvidence(invocation store.Invocation) (bool, error) {
	if strings.TrimSpace(invocation.NativeSessionID) != "" {
		return false, nil
	}
	presence, err := structuredReportPresenceForInvocation(invocation)
	if err != nil {
		return false, err
	}
	return presence == structuredReportAbsent, nil
}

// failedLaunchRetryGuidance identifies a launch that left no evidence to
// protect and tells the operator which command reopens its role boundary.
func failedLaunchRetryGuidance(invocation store.Invocation) string {
	return fmt.Sprintf("%s for %s/%s; use `/factory retry` to start a fresh invocation", failedLaunchVoidMarker, invocation.Role, invocation.Stage)
}

// promptForPersistedInvocation rebuilds the same role prompt from the frozen
// packet and invocation packet after a process restart.
func promptForPersistedInvocation(run store.Run, invocation store.Invocation, packet SpecificationPacket) (string, error) {
	data, err := os.ReadFile(filepath.Join(invocation.InvocationDirectory, invocationPacketFileName))
	if err != nil {
		return "", fmt.Errorf("read persisted invocation packet: %w", err)
	}
	persisted, err := decodePersistedInvocationPacket(data)
	if err != nil {
		return "", fmt.Errorf("decode persisted invocation packet: %w", err)
	}
	if err := validatePersistedInvocationPacket(run, invocation, persisted); err != nil {
		return "", err
	}
	var repositoryCraft *RepositoryCraftDocument
	if persisted.SchemaVersion >= 9 || len(packet.RepositoryConfig.RoleCraft) > 0 || invocation.PromptCraftSourcePath != "" || invocation.PromptCraftSHA256 != "" {
		repositoryCraft, err = validateInvocationCraftIdentity(packet, persisted, invocation)
		if err != nil {
			return "", err
		}
	}
	rebuiltSpecification, err := promptSpecification(packet)
	if err != nil {
		return "", err
	}
	return prompt.Build(prompt.Request{
		InvocationID:            invocation.ID,
		RunID:                   run.ID,
		Role:                    invocation.Role,
		Stage:                   string(invocation.Stage),
		SpecificationPacket:     rebuiltSpecification,
		Continuation:            persisted.Continuation,
		RepositoryGuidance:      packet.RepositoryGuidance,
		RepositoryCraft:         repositoryCraftContent(repositoryCraft),
		PromptVersion:           invocation.PromptVersion,
		TestPolicyMode:          string(packet.RepositoryConfig.TestPolicy.Mode),
		Route:                   packet.Route,
		DesignHandoff:           persisted.DesignHandoff,
		TestHandoff:             persisted.TestHandoff,
		TestObjection:           persisted.TestObjection,
		TestRevisionAttempt:     persisted.TestRevisionAttempt,
		TestRevisionBudget:      persisted.TestRevisionBudget,
		ProtectedTestPaths:      persisted.ProtectedTestPaths,
		TestExemption:           persisted.TestExemption,
		ReviewRepair:            persisted.ReviewRepair,
		TestPaths:               packet.RepositoryConfig.TestPolicy.TestPaths,
		TestInfrastructurePaths: packet.RepositoryConfig.TestPolicy.InfrastructurePaths,
		CheckRepairAttempt:      checkRepairAttempt(persisted.CheckRepair),
		CheckRepairBudget:       checkRepairBudget(persisted.CheckRepair),
		ReviewContext:           persisted.ReviewContext,
	})
}

// decodePersistedInvocationPacket accepts the current and retained historical
// append-only packet shapes. Fields introduced after an older version remain at
// their zero value, while missing, future, and unsupported versions fail closed.
func decodePersistedInvocationPacket(data []byte) (InvocationPacket, error) {
	var persisted InvocationPacket
	if err := json.Unmarshal(data, &persisted); err != nil {
		return InvocationPacket{}, err
	}
	if !supportedInvocationPacketVersion(persisted.SchemaVersion) {
		return InvocationPacket{}, fmt.Errorf("unsupported persisted invocation packet schema version %d", persisted.SchemaVersion)
	}
	return persisted, nil
}

// supportedInvocationPacketVersion reports whether a packet belongs to the
// contiguous compatibility range maintained by the current factory.
func supportedInvocationPacketVersion(version int) bool {
	return version >= invocationPacketMinimumSupportedVersion && version <= invocationPacketVersion
}

// validatePersistedInvocationPacket verifies that a decoded packet still
// describes the durable invocation and frozen run being resumed. Review
// invocations additionally require their exact review context to be present.
func validatePersistedInvocationPacket(run store.Run, invocation store.Invocation, persisted InvocationPacket) error {
	if persisted.InvocationID != invocation.ID || persisted.RunID != run.ID {
		return errors.New("persisted invocation packet identity does not match the run")
	}
	if strings.TrimSpace(persisted.Role) == "" || strings.TrimSpace(string(persisted.Stage)) == "" {
		return errors.New("persisted invocation packet role and stage are required")
	}
	if persisted.Role != invocation.Role || persisted.Stage != invocation.Stage {
		return errors.New("persisted invocation packet role and stage do not match the invocation")
	}
	if strings.TrimSpace(persisted.SpecificationPacket) == "" {
		return errors.New("persisted invocation packet specification is required")
	}
	if persisted.SpecificationPacket != run.SpecificationPacket {
		return errors.New("persisted invocation packet specification does not match the run")
	}
	if strings.TrimSpace(persisted.PromptVersion) == "" {
		return errors.New("persisted invocation packet prompt version is required")
	}
	if invocation.PromptVersion != "" && persisted.PromptVersion != invocation.PromptVersion {
		return errors.New("persisted invocation packet prompt version does not match the invocation")
	}
	if persisted.SchemaVersion >= 9 {
		if persisted.PromptCraftSourcePath != invocation.PromptCraftSourcePath {
			return &CraftSourcePathMismatchError{Role: invocation.Role, InvocationID: invocation.ID, Expected: invocation.PromptCraftSourcePath, Observed: persisted.PromptCraftSourcePath}
		}
		if persisted.PromptCraftSHA256 != invocation.PromptCraftSHA256 {
			return &CraftDigestMismatchError{Role: invocation.Role, InvocationID: invocation.ID, SourcePath: invocation.PromptCraftSourcePath, Expected: invocation.PromptCraftSHA256, Observed: persisted.PromptCraftSHA256}
		}
	} else if invocation.PromptCraftSourcePath != "" || invocation.PromptCraftSHA256 != "" {
		return errors.New("legacy persisted invocation packet cannot carry repository craft identity")
	}
	if persisted.TestPolicyMode != "" {
		packet, err := decodeSpecificationPacket(run.SpecificationPacket)
		if err != nil {
			return fmt.Errorf("decode persisted invocation test policy: %w", err)
		}
		if persisted.TestPolicyMode != packet.RepositoryConfig.TestPolicy.Mode {
			return errors.New("persisted invocation packet test policy does not match the frozen repository policy")
		}
		if persisted.Route != packet.Route {
			return errors.New("persisted invocation packet route does not match the frozen specification packet route")
		}
	}
	if persisted.SchemaVersion >= 7 {
		if !sameTestObjection(persisted.TestObjection, run.TestObjection) {
			return errors.New("persisted invocation packet test objection does not match the run")
		}
		if run.TestObjection != nil && (persisted.TestRevisionAttempt != run.TestRevisionAttempts || persisted.TestRevisionBudget != run.TestRevisionBudget) {
			return errors.New("persisted invocation packet test revision state does not match the run")
		}
	}
	if persisted.SchemaVersion >= 8 {
		expectedReviewRepair := run.ReviewRepairPacket
		if invocation.Role != workflow.RoleImplementation {
			expectedReviewRepair = nil
		}
		if !sameReviewRepairPacket(persisted.ReviewRepair, expectedReviewRepair) {
			return errors.New("persisted invocation packet review-repair context does not match the run")
		}
	}
	if roleDefinition, ok := workflow.DefaultRegistry().Role(invocation.Role); ok && roleDefinition.Kind == workflow.RoleKindReview {
		if persisted.ReviewContext == nil {
			return fmt.Errorf("persisted %s invocation packet has no review context", invocation.Role)
		}
		if strings.TrimSpace(persisted.ReviewContext.CheckpointSHA) == "" {
			return fmt.Errorf("persisted %s invocation packet has no review checkpoint", invocation.Role)
		}
		if persisted.ReviewContext.CheckpointSHA != run.CheckpointSHA {
			return fmt.Errorf("persisted %s invocation packet review checkpoint does not match the run", invocation.Role)
		}
	}
	return nil
}

// sameReviewRepairPacket compares the coordinator-owned repair context without
// relying on pointer identity across a restart.
func sameReviewRepairPacket(left, right *store.ReviewRepairPacket) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

// sameTestObjection compares the coordinator-owned objection payload without
// exposing pointer identity as part of restart validation.
func sameTestObjection(left, right *store.TestObjection) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Test == right.Test && left.Claim == right.Claim && left.Evidence == right.Evidence && left.InvocationID == right.InvocationID
}

// checkRepairAttempt extracts the bounded repair attempt from a persisted
// invocation packet while keeping the prompt package independent of factory
// packet types.
func checkRepairAttempt(packet *CheckRepairPacket) int {
	if packet == nil {
		return 0
	}
	return packet.Attempt
}

// checkRepairBudget extracts the configured repair budget from a persisted
// invocation packet.
func checkRepairBudget(packet *CheckRepairPacket) int {
	if packet == nil {
		return 0
	}
	return packet.Budget
}

// workerImageMatches accepts Docker's image@digest formatting while requiring
// the frozen digest whenever the runtime exposes an image identity.
func workerImageMatches(observed, image, digest string) bool {
	observed = strings.TrimSpace(observed)
	if observed == "" {
		return true
	}
	if strings.TrimSpace(digest) == "" {
		return observed == image
	}
	return observed == image+"@"+digest || strings.HasSuffix(observed, "@"+digest)
}

// reviewCheckpointSHA supplies an exact checkpoint only to the review harness.
func reviewCheckpointSHA(review bool, checkpoint string) string {
	if !review {
		return ""
	}
	return checkpoint
}

// credentialProjectionError reports a credential restoration refusal without
// retaining a host path, credential contents, or adapter output.
type credentialProjectionError struct {
	// Harness identifies the non-secret adapter whose projection is unavailable.
	Harness string
}

// Error returns bounded operator guidance for restoring the managed projection.
func (e *credentialProjectionError) Error() string {
	if e == nil {
		return "credential projection could not be restored; retry `factory auth refresh`"
	}
	return fmt.Sprintf("%s credential projection could not be restored; restore the configured source and retry `factory auth refresh`", credentialHarnessLabel(e.Harness))
}

// Unwrap lets the normal recovery policy pause the run in the auth state.
func (e *credentialProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return harness.NewAuthenticationExpiredError(credentialHarnessLabel(e.Harness))
}

// newCredentialProjectionError constructs the redacted credential recovery
// error used at every host-source and worker-projection boundary.
func newCredentialProjectionError(harnessName string) error {
	return &credentialProjectionError{Harness: credentialHarnessLabel(harnessName)}
}

// credentialHarnessLabel bounds the adapter identity included in recovery
// guidance so malformed persisted data cannot become an output channel.
func credentialHarnessLabel(harnessName string) string {
	switch config.Harness(harnessName) {
	case config.HarnessCodex:
		return string(config.HarnessCodex)
	case config.HarnessClaude:
		return string(config.HarnessClaude)
	default:
		return "harness"
	}
}

// harnessFailureDiagnosticName is the invocation-local file that records what a
// harness printed before the coordinator tore its session down.
const harnessFailureDiagnosticName = "harness-failure.log"

// harnessTranscriptLines bounds a diagnostic surface read, matching the bound
// the harness adapter applies when it captures a failing launch.
const harnessTranscriptLines = 200

// writeHarnessFailureDiagnostic records what a harness printed beside its
// invocation and returns the path it wrote, or an empty string when there was
// nothing to record. The occasion names why the coordinator captured it, so a
// launch that never started reads differently from a session that exited.
//
// The file stays on the coordinator host: it holds harness output, which the
// operator-visible run projections deliberately exclude. Writing it is
// best-effort, because losing a diagnostic must never mask the failure the
// caller is already reporting.
func writeHarnessFailureDiagnostic(invocationRoot, occasion string, cause error, transcript string, observedAt time.Time) string {
	if strings.TrimSpace(invocationRoot) == "" || strings.TrimSpace(transcript) == "" {
		return ""
	}
	path := filepath.Join(invocationRoot, harnessFailureDiagnosticName)
	body := fmt.Sprintf("observed at: %s\noccasion: %s\ncause: %v\n\nsurface output:\n%s\n", observedAt.Format(time.RFC3339), occasion, cause, transcript)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return ""
	}
	return path
}

// captureSurfaceTranscript reads a surface's recent output when the terminal
// adapter exposes that capability. It is the coordinator-side counterpart to
// the harness adapter's launch capture, used where the coordinator, not the
// adapter, is the first to observe that a session is gone.
func captureSurfaceTranscript(ctx context.Context, runtime terminal.TerminalRuntime, surfaceID terminal.SurfaceID) string {
	reader, readable := runtime.(terminal.SurfaceReader)
	if !readable || strings.TrimSpace(string(surfaceID)) == "" {
		return ""
	}
	captureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	transcript, err := reader.ReadSurface(captureContext, surfaceID, harnessTranscriptLines)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(transcript)
}

// invocationRoot returns the private per-invocation directory holding one
// invocation's packet, results, and diagnostics.
func invocationRoot(run store.Run, invocationID string) string {
	return filepath.Join(filepath.Dir(run.Worktree), ".factory-agents", run.ID, invocationID)
}
