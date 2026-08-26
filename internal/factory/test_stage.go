package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// testExemptionMarkerPrefix identifies the exact issue marker that authorizes
// a human test-stage skip.
const testExemptionMarkerPrefix = "<!-- factory-test-exemption:"

// testStageConfigured reports whether the frozen repository policy declares a
// usable test role. Repository configuration validation requires this role for
// every operational packet, so a missing role is a fail-closed invalid fixture.
func testStageConfigured(repository config.RepositoryConfig) bool {
	if repository.RoleHarnessDefaults == nil || repository.ModelOptions == nil {
		return false
	}
	_, hasHarness := repository.RoleHarnessDefaults["test"]
	models, hasModels := repository.ModelOptions["test"]
	return hasHarness && hasModels && len(models) > 0
}

// testStageHumanExemption returns the frozen human exemption projection when
// the issue marker and repository policy authorize it.
func testStageHumanExemption(packet SpecificationPacket) (*store.TestExemption, bool) {
	if !packet.RepositoryConfig.TestPolicy.AllowHumanExemption {
		return nil, false
	}
	justification, ok := parseHumanTestExemption(packet.Issue.Body)
	if !ok {
		return nil, false
	}
	return &store.TestExemption{Kind: "human", Justification: justification}, true
}

// testStageShouldRun reports whether the frozen packet requires a visible test
// role before implementation.
func testStageShouldRun(packet SpecificationPacket) bool {
	_, exempted := testStageHumanExemption(packet)
	return !exempted
}

// validateTestStageTransition prevents a generic coordinator transition from
// bypassing the configured test handoff or inventing an unconfigured test role.
func validateTestStageTransition(run store.Run, packet SpecificationPacket, requested store.Stage) error {
	configured := testStageConfigured(packet.RepositoryConfig)
	if !configured {
		return errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	if requested == store.StageTest {
		if !testStageShouldRun(packet) {
			return errors.New("test stage is pre-authorized for human exemption")
		}
		return nil
	}
	if requested != store.StageImplementation {
		return nil
	}
	if run.TestStageSkipped {
		if run.TestExemption == nil {
			return errors.New("test-stage skip requires a recorded exemption")
		}
		return nil
	}
	if run.TestHandoff == nil || strings.TrimSpace(run.TestCheckpointSHA) == "" {
		return errors.New("implementation stage requires a completed test handoff or recorded test-stage exemption")
	}
	return nil
}

// advanceAfterBaseline moves a configured run to test stage, or records an
// authorized skip before implementation. The transition is persisted through
// the same status-comment boundary as every other visible run transition.
func (s *Service) advanceAfterBaseline(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket) (store.Run, error) {
	if !testStageConfigured(packet.RepositoryConfig) {
		return run, errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	next := run
	skip := !testStageShouldRun(packet)
	justification := ""
	if exemption, ok := testStageHumanExemption(packet); ok {
		justification = exemption.Justification
		next.TestExemption = exemption
	}
	if skip {
		next.TestStageSkipped = true
		next.Stage = store.StageImplementation
		if justification == "" {
			next.TestExemption = nil
			next.LifecycleReason = "test stage skip requires an authorized human exemption"
		} else {
			next.LifecycleReason = "test stage skipped by human exemption"
		}
	} else {
		next.Stage = store.StageTest
		next.TestStageSkipped = false
		next.LifecycleReason = "baseline passed; test stage ready"
	}
	next.Status = store.StatusActive
	next.UpdatedAt = s.deps.Now().UTC()
	err := s.persistAgentRunState(ctx, registration, runStore, run, next)
	if err != nil {
		return run, fmt.Errorf("persist post-baseline test-stage transition: %w", err)
	}
	updated := next
	if next.TestExemption != nil {
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recorder.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemptionHuman); err != nil {
				return updated, fmt.Errorf("record test-stage human exemption: %w", err)
			}
		}
	}
	return updated, nil
}

// parseHumanTestExemption recognizes the exact frozen issue marker used to
// pre-authorize a test-stage skip. Ordinary issue prose cannot authorize one.
func parseHumanTestExemption(body string) (string, bool) {
	start := indexFold(body, testExemptionMarkerPrefix)
	if start < 0 {
		return "", false
	}
	contentStart := start + len(testExemptionMarkerPrefix)
	remainder := body[contentStart:]
	end := strings.Index(remainder, "-->")
	if end < 0 {
		return "", false
	}
	content := strings.TrimSpace(remainder[:end])
	parts := strings.SplitN(content, "|", 2)
	if len(parts) != 2 {
		parts = strings.SplitN(content, ":", 2)
	}
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "human") {
		return "", false
	}
	justification := strings.TrimSpace(parts[1])
	if justification == "" || utf8.RuneCountInString(justification) > 4000 || strings.ContainsAny(justification, "\x00\r\n") {
		return "", false
	}
	return justification, true
}

// indexFold returns the index of the first case-insensitive occurrence of
// substr in s, or -1 if substr is not present in s.
func indexFold(s, substr string) int {
	substrLen := len(substr)
	if substrLen == 0 {
		return 0
	}
	if substrLen > len(s) {
		return -1
	}
	lowerSubstr := strings.ToLower(substr)
	for i := 0; i <= len(s)-substrLen; i++ {
		if strings.EqualFold(s[i:i+substrLen], lowerSubstr) {
			return i
		}
	}
	return -1
}

// validateTestStageReportPolicy rejects harness-reported exemptions that the
// frozen repository policy does not authorize.
func validateTestStageReportPolicy(value report.Report, policy config.TestPolicy) error {
	if value.Role != "test" || value.Stage != string(store.StageTest) {
		return nil
	}
	for _, exemption := range value.Exemptions {
		switch exemption {
		case report.ExemptionTechnical:
			if !policy.AllowTechnicalExemption {
				return errors.New("technical test-stage exemption is not allowed by repository policy")
			}
		case report.ExemptionHuman:
			return errors.New("human test-stage exemption must be pre-authorized in the frozen issue")
		}
	}
	if value.Outcome == report.OutcomeCompleted && containsReportExemption(value.Exemptions, report.ExemptionTechnical) && value.TestHandoff != nil {
		return errors.New("technical test-stage exemption must not include a test handoff")
	}
	return nil
}

// containsReportExemption reports whether a report carries one fixed exemption.
func containsReportExemption(values []report.Exemption, wanted report.Exemption) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// isUnverifiableTestReport identifies a completed test envelope whose identity
// is trustworthy enough to enter the required human-disposition path after
// payload validation fails.
func isUnverifiableTestReport(value report.Report, invocation store.Invocation) bool {
	return invocation.Stage == store.StageTest &&
		value.SchemaVersion == report.SchemaVersion &&
		value.InvocationID == invocation.ID &&
		value.RunID == invocation.RunID &&
		value.Harness == invocation.Harness &&
		value.Role == "test" &&
		value.Stage == string(store.StageTest) &&
		value.Outcome == report.OutcomeCompleted
}

// validateTestChangedPaths enforces the test policy against the complete Git
// observation, so omitting a production path from the report cannot hide it.
func validateTestChangedPaths(paths []string, policy config.TestPolicy) error {
	for _, path := range paths {
		clean, err := cleanTestPath(path)
		if err != nil {
			return err
		}
		if !testOwnedPath(clean, policy) {
			return fmt.Errorf("test stage changed path %q is outside protected test paths", path)
		}
	}
	return nil
}

// cleanTestPath validates and normalizes a Git-observed repository path.
func cleanTestPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\x00\r\n\\") || filepath.IsAbs(path) {
		return "", fmt.Errorf("test stage changed path %q is not repository-relative", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("test stage changed path %q escapes the repository", path)
	}
	return clean, nil
}

// testOwnedPath applies conventional test ownership and repository policy
// prefixes to one normalized path.
func testOwnedPath(path string, policy config.TestPolicy) bool {
	if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "test/") || strings.HasPrefix(path, "tests/") || strings.HasPrefix(path, "test-support/") || strings.HasPrefix(path, "__tests__/") {
		return true
	}
	for _, prefix := range append(append([]string(nil), policy.TestPaths...), policy.InfrastructurePaths...) {
		clean, err := cleanTestPath(prefix)
		if err == nil && (path == clean || strings.HasPrefix(path, clean+"/")) {
			return true
		}
	}
	return false
}

// validateProtectedTestPaths verifies implementation has not changed a
// protected test path since the test checkpoint, including a deleted file.
func validateProtectedTestPaths(worktree string, state gitadapter.WorktreeState, protected []store.ProtectedTestPath) error {
	for _, value := range protected {
		current, err := testPathDigest(worktree, value.Path)
		if err != nil {
			return fmt.Errorf("inspect protected test path %q: %w", value.Path, err)
		}
		if current != value.SHA256 {
			return fmt.Errorf("implementation changed protected test path %q", value.Path)
		}
		for _, changed := range state.ChangedPaths {
			clean, cleanErr := cleanTestPath(changed)
			if cleanErr == nil && clean == value.Path {
				return fmt.Errorf("implementation changed protected test path %q", value.Path)
			}
		}
	}
	return nil
}

// testPathDigest records the content identity used for protected test paths.
func testPathDigest(worktree, relative string) (string, error) {
	clean, err := cleanTestPath(relative)
	if err != nil {
		return "", err
	}
	path := filepath.Join(worktree, filepath.FromSlash(clean))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		hash := sha256.Sum256([]byte("factory-deleted:" + clean))
		return hex.EncodeToString(hash[:]), nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("protected test path is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// protectedTestPathsForCheckpoint records hashes after the scoped test commit.
func protectedTestPathsForCheckpoint(worktree string, paths []string) ([]store.ProtectedTestPath, error) {
	protected := make([]store.ProtectedTestPath, 0, len(paths))
	for _, path := range paths {
		clean, err := cleanTestPath(path)
		if err != nil {
			return nil, err
		}
		digest, err := testPathDigest(worktree, clean)
		if err != nil {
			return nil, fmt.Errorf("hash test path %q: %w", clean, err)
		}
		protected = append(protected, store.ProtectedTestPath{Path: clean, SHA256: digest})
	}
	return protected, nil
}

// acceptTestStageReport verifies a reported red test, creates its separate
// checkpoint, persists protected paths, and launches implementation only after
// the handoff is durable.
func (s *Service) acceptTestStageReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report, state gitadapter.WorktreeState) (AgentResult, error) {
	_, ok := runStore.(InvocationStore)
	if !ok {
		return AgentResult{}, errors.New("operational store does not support visible invocations")
	}
	if err := validateTestChangedPaths(state.ChangedPaths, decodeTestPolicy(run.SpecificationPacket)); err != nil {
		if stopErr := s.stopRunWorker(ctx, run.ID); stopErr != nil {
			return AgentResult{}, stopErr
		}
		return s.pauseTestForHuman(ctx, registration, runStore, run, invocation, value, "test-stage path ownership dispute")
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		previous := *run
		run.Status = store.StatusWaitingForHuman
		run.Stage = store.StageTest
		run.PendingQuestions = pendingQuestionsFromReport(value.Questions)
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
		run.LifecycleReason = "test agent requested clarification"
		run.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
			return AgentResult{}, fmt.Errorf("persist test clarification state: %w", err)
		}
		published, err := s.ensureClarificationPublication(ctx, registration, runStore, *run)
		if err != nil {
			return AgentResult{}, err
		}
		*run = published
		return AgentResult{Invocation: *invocation, Report: value}, nil
	}

	if value.Outcome == report.OutcomeCannotProceed {
		if err := s.stopRunWorker(ctx, run.ID); err != nil {
			return AgentResult{}, err
		}
		return s.pauseTestForHuman(ctx, registration, runStore, run, invocation, value, "test agent cannot proceed")
	}

	if containsReportExemption(value.Exemptions, report.ExemptionTechnical) {
		if err := s.stopRunWorker(ctx, run.ID); err != nil {
			return AgentResult{}, err
		}
		run.TestExemption = &store.TestExemption{Kind: "technical", Justification: value.Summary}
		return s.launchImplementationAfterTest(ctx, registration, runStore, run, invocation, value)
	}
	if value.TestHandoff == nil {
		return AgentResult{}, errors.New("completed test report requires a test handoff")
	}
	if strings.ContainsAny(value.TestHandoff.FocusedTestCommand, "\x00\r\n") {
		return AgentResult{}, errors.New("focused test command contains a control character")
	}
	if s.deps.Worker == nil {
		return AgentResult{}, errors.New("worker runtime is required for red-test verification")
	}
	result, err := s.deps.Worker.RunCommand(ctx, worker.CommandRequest{
		RunID:             run.ID,
		Command:           value.TestHandoff.FocusedTestCommand,
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              "test",
	})
	stopErr := s.stopRunWorker(ctx, run.ID)
	output := result.Stdout + "\n" + result.Stderr
	if err != nil || stopErr != nil || result.ExitCode == 0 || !strings.Contains(output, value.TestHandoff.ExpectedFailureReason) {
		if stopErr != nil && err == nil {
			err = stopErr
		}
		reason := "focused red-test verification is disputed or unverifiable"
		if err != nil {
			reason = "focused red-test verification could not be completed"
		} else if result.ExitCode != 0 && !strings.Contains(output, value.TestHandoff.ExpectedFailureReason) {
			reason = "focused red-test output did not contain the expected failure reason"
		}
		return s.pauseTestForHuman(ctx, registration, runStore, run, invocation, value, reason)
	}

	inspector := s.worktreeInspector()
	if inspector == nil {
		return AgentResult{}, errors.New("worktree inspector is required after red-test verification")
	}
	verifiedState, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		return AgentResult{}, fmt.Errorf("inspect worktree after red-test verification: %w", err)
	}
	if verifiedState.HeadSHA != run.CheckpointSHA {
		return AgentResult{}, fmt.Errorf("test-stage worktree HEAD %q changed during red-test verification", verifiedState.HeadSHA)
	}
	if err := validateTestChangedPaths(verifiedState.ChangedPaths, decodeTestPolicy(run.SpecificationPacket)); err != nil {
		return s.pauseTestForHuman(ctx, registration, runStore, run, invocation, value, "test-stage path ownership dispute")
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return AgentResult{}, errors.New("GitWorkspace is required for the test checkpoint")
	}
	checkpoint, err := workspace.CreateCheckpoint(ctx, gitadapter.CheckpointRequest{
		RunID:        run.ID,
		WorktreePath: run.Worktree,
		ParentSHA:    run.CheckpointSHA,
		Kind:         gitadapter.CheckpointKindTest,
		Paths:        append([]string(nil), verifiedState.ChangedPaths...),
		Message:      "verified focused test is red",
	})
	if err != nil {
		return AgentResult{}, fmt.Errorf("create test checkpoint: %w", err)
	}
	protected, err := protectedTestPathsForCheckpoint(run.Worktree, verifiedState.ChangedPaths)
	if err != nil {
		return AgentResult{}, err
	}
	handoff := convertTestHandoff(*value.TestHandoff)
	run.TestCheckpointSHA = checkpoint.SHA
	run.CheckpointSHA = checkpoint.SHA
	run.SpecificationReview = nil
	run.TestHandoff = &handoff
	run.ProtectedTestPaths = protected
	run.Stage = store.StageImplementation
	run.Status = store.StatusActive
	run.PendingQuestions = nil
	run.LifecycleReason = "test stage completed; protected handoff ready for implementation"
	run.UpdatedAt = s.deps.Now().UTC()
	previous := *run
	previous.Stage = store.StageTest
	previous.Status = store.StatusActive
	previous.CheckpointSHA = verifiedState.HeadSHA
	previous.TestCheckpointSHA = ""
	previous.TestHandoff = nil
	previous.ProtectedTestPaths = nil
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist test handoff: %w", err)
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{RunID: run.ID, Role: "implementation", Stage: store.StageImplementation}); err != nil {
		return AgentResult{}, fmt.Errorf("launch implementation after test handoff: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// pauseUnverifiableTestReport stops the visible test execution before placing
// a structurally invalid completed test report in human disposition.
func (s *Service) pauseUnverifiableTestReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if err := s.stopRunWorker(ctx, run.ID); err != nil {
		return AgentResult{}, err
	}
	if invocation.NativeSessionID != "" {
		_, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath)
		if err != nil {
			return AgentResult{}, fmt.Errorf("ensure agent runtime for unverifiable test report: %w", err)
		}
		if err := harnessRuntime.Finish(ctx, harness.Session{
			InvocationID:    invocation.ID,
			NativeSessionID: invocation.NativeSessionID,
			Surface: terminal.Surface{
				ID:          terminal.SurfaceID(invocation.ImplementationSurfaceID),
				WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
				Name:        invocation.Role,
			},
		}); err != nil {
			return AgentResult{}, fmt.Errorf("finish unverifiable test session: %w", err)
		}
	}
	return s.pauseTestForHuman(ctx, registration, runStore, run, invocation, value, "focused red-test report is unverifiable")
}

// pauseTestForHuman persists a content-free test dispute without starting an
// automated revision loop before the human-disposition protocol exists.
func (s *Service) pauseTestForHuman(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report, reason string) (AgentResult, error) {
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationTestDispute); err != nil {
			return AgentResult{}, fmt.Errorf("record test-stage dispute: %w", err)
		}
	}
	invocation.Status = store.InvocationStatusWaitingForHuman
	invocation.UpdatedAt = s.deps.Now().UTC()
	if invocationStore, ok := runStore.(InvocationStore); ok {
		if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
			return AgentResult{}, fmt.Errorf("persist disputed test invocation: %w", err)
		}
	}
	previous := *run
	run.Stage = store.StageTest
	run.Status = store.StatusWaitingForHuman
	run.PendingQuestions = nil
	run.LifecycleReason = reason
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist test dispute state: %w", err)
	}
	if err := s.notifyWorkspace(ctx, registration, "factory test stage waiting", run.ID+" is waiting for human test-stage disposition"); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// launchImplementationAfterTest records an exemption and immediately hands
// the run to implementation using the same open store and frozen packet.
func (s *Service) launchImplementationAfterTest(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	previous := *run
	run.Stage = store.StageImplementation
	run.Status = store.StatusActive
	run.TestStageSkipped = true
	run.PendingQuestions = nil
	run.LifecycleReason = "test stage completed with technical exemption"
	run.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist technical test exemption: %w", err)
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemptionTechnical); err != nil {
			return AgentResult{}, err
		}
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{RunID: run.ID, Role: "implementation", Stage: store.StageImplementation}); err != nil {
		return AgentResult{}, fmt.Errorf("launch implementation after technical exemption: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// convertTestHandoff copies the report-layer envelope into store-owned durable
// types without retaining report package coupling in the operational schema.
func convertTestHandoff(value report.TestHandoff) store.TestHandoff {
	handoff := store.TestHandoff{
		ChangedFiles:          append([]string(nil), value.ChangedFiles...),
		FocusedTestCommand:    value.FocusedTestCommand,
		ExpectedFailureReason: value.ExpectedFailureReason,
		InfrastructureChanges: append([]string(nil), value.InfrastructureChanges...),
		UncoveredCriteria:     append([]string(nil), value.UncoveredCriteria...),
	}
	for _, coverage := range value.AcceptanceCoverage {
		handoff.AcceptanceCoverage = append(handoff.AcceptanceCoverage, store.TestAcceptanceCoverage{Criterion: coverage.Criterion, Evidence: coverage.Evidence})
	}
	for _, evidence := range value.ObservedFailureEvidence {
		handoff.ObservedFailureEvidence = append(handoff.ObservedFailureEvidence, store.TestFailureEvidence{Kind: evidence.Kind, Detail: evidence.Detail})
	}
	return handoff
}

// decodeTestPolicy loads the frozen test policy for path authorization.
func decodeTestPolicy(specification string) config.TestPolicy {
	packet, err := decodeSpecificationPacket(specification)
	if err != nil {
		return config.TestPolicy{}
	}
	return packet.RepositoryConfig.TestPolicy
}
