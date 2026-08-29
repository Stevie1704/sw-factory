package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// testExemptionMarkerPrefix identifies the exact issue marker that authorizes
// a human test-stage skip.
const testExemptionMarkerPrefix = "<!-- factory-test-exemption:"

// measuredPilotIssueNumber is the fixed evidence-gate issue named by the
// objection-cycle specification.
const measuredPilotIssueNumber = 26

// testPolicyModeForRun returns the frozen test-policy mode used by status and
// command projections. Invalid historical packets return the empty mode so a
// projection cannot present an unverified policy as authoritative.
func testPolicyModeForRun(run store.Run) config.TestMode {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return ""
	}
	return packet.RepositoryConfig.TestPolicy.Mode
}

// testPolicyDescription renders the stable user-facing meaning of a test
// policy mode without treating advisory implementation ownership as an
// exemption.
func testPolicyDescription(mode config.TestMode) string {
	switch mode {
	case config.TestModeAdvisory:
		return "advisory (implementation-owned TDD)"
	case config.TestModeRequired:
		return "required (independent test stage)"
	default:
		return "unknown"
	}
}

// TestPolicyDescription exposes the stable user-facing meaning of a test
// policy mode to adapters such as the CLI without exposing packet internals.
func TestPolicyDescription(mode config.TestMode) string { return testPolicyDescription(mode) }

// implementationStartIsClean reports whether an implementation invocation may
// begin without a separate test handoff. Required-mode skips retain their
// recorded exemption, while the implementation-owned path has no skip marker
// at all.
func implementationStartIsClean(run store.Run) bool {
	if run.TestStageSkipped {
		return true
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	return err == nil && !independentTestStageDeclared(packet)
}

// independentTestStageDeclared reports whether the frozen packet places a
// factory-owned test role before implementation. Required repository policy
// always declares one, and a selected contract-first route declares one even
// under advisory policy.
func independentTestStageDeclared(packet SpecificationPacket) bool {
	return packet.Route.RequiresIndependentTestStage() || packet.RepositoryConfig.TestPolicy.Mode == config.TestModeRequired
}

// testRolePolicySatisfied reports whether the frozen packet declares the
// test-role harness and model policy it needs. A packet with no independent
// test stage needs none, because implementation owns its own TDD loop.
func testRolePolicySatisfied(packet SpecificationPacket) bool {
	mode := packet.RepositoryConfig.TestPolicy.Mode
	if mode != config.TestModeAdvisory && mode != config.TestModeRequired {
		return false
	}
	if !independentTestStageDeclared(packet) {
		return true
	}
	return roleConfigured(packet.RepositoryConfig, workflow.RoleTest)
}

// postBaselineStage returns the stage a healthy baseline enters: the selected
// route's entry stage when the issue chose one, and otherwise the stage the
// frozen repository test policy selects.
func postBaselineStage(packet SpecificationPacket) store.Stage {
	if entry, ok := packet.Route.EntryStage(); ok {
		return entry
	}
	if independentTestStageDeclared(packet) {
		return store.StageTest
	}
	return store.StageImplementation
}

// testStageHumanExemption returns the frozen human exemption projection when
// the issue marker and repository policy authorize it. A selected
// contract-first route is a deliberate request for independently authored
// acceptance evidence, so it overrides the skip marker.
func testStageHumanExemption(packet SpecificationPacket) (*store.TestExemption, bool) {
	if packet.Route.RequiresIndependentTestStage() {
		return nil, false
	}
	if packet.RepositoryConfig.TestPolicy.Mode != config.TestModeRequired {
		return nil, false
	}
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
	if !independentTestStageDeclared(packet) {
		return false
	}
	_, exempted := testStageHumanExemption(packet)
	return !exempted
}

// validateTestStageTransition prevents a generic coordinator transition from
// bypassing the configured test handoff or inventing an unconfigured test role.
func validateTestStageTransition(run store.Run, packet SpecificationPacket, requested store.Stage) error {
	switch packet.RepositoryConfig.TestPolicy.Mode {
	case config.TestModeAdvisory, config.TestModeRequired:
		// Continue with the route- and policy-aware checks below.
	default:
		return errors.New("frozen repository packet has an unsupported test policy mode")
	}
	if !independentTestStageDeclared(packet) {
		if requested == store.StageTest {
			return errors.New("test stage is unavailable in advisory mode without a selected route; implementation owns TDD")
		}
		return nil
	}
	if packet.Route == workflow.RouteDesignAcceptance && run.Stage == store.StageClaim {
		return fmt.Errorf("route %q requires the architecture stage before %q", packet.Route, requested)
	}
	configured := testRolePolicySatisfied(packet)
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

// advanceAfterBaseline moves a run to the stage its frozen route and test
// policy select: architecture for the design-acceptance route, the independent
// test stage for the acceptance route or required policy, and
// implementation-owned TDD otherwise. The transition is persisted through the
// same status-comment boundary as every other visible run transition.
func (s *Service) advanceAfterBaseline(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket) (store.Run, error) {
	if !testRolePolicySatisfied(packet) {
		return run, errors.New("frozen repository packet lacks the mandatory test-stage role policy")
	}
	next := run
	if entry := postBaselineStage(packet); entry != store.StageTest {
		next.Stage = entry
		next.Status = store.StatusActive
		next.TestHandoff = nil
		next.TestCheckpointSHA = ""
		next.ProtectedTestPaths = nil
		next.TestExemption = nil
		next.TestStageSkipped = false
		next.LifecycleReason = "baseline passed; " + postBaselineReason(entry)
		next.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
			return run, fmt.Errorf("persist post-baseline %q transition: %w", entry, err)
		}
		return next, nil
	}
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

// postBaselineReason renders the visible lifecycle reason for the stage a
// healthy baseline entered.
func postBaselineReason(stage store.Stage) string {
	if stage == store.StageArchitecture {
		return "architecture stage ready"
	}
	return "implementation-owned TDD ready"
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
	if value.Role != workflow.RoleTest || value.Stage != string(store.StageTest) {
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

// validateImplementationOwnedSignals rejects test-stage-only dispositions from
// the implementation contract when no independent test stage exists and
// implementation owns the TDD loop.
func validateImplementationOwnedSignals(value report.Report, invocation store.Invocation, packet SpecificationPacket) error {
	if invocation.Role != workflow.RoleImplementation || independentTestStageDeclared(packet) {
		return nil
	}
	for _, exemption := range value.Exemptions {
		switch exemption {
		case report.ExemptionHuman, report.ExemptionTechnical, report.ExemptionBaseline:
			return errors.New("advisory implementation reports cannot record test-stage exemptions")
		}
	}
	for _, escalation := range value.Escalations {
		if escalation == report.EscalationTestDispute {
			return errors.New("advisory implementation reports cannot record test-stage disputes")
		}
	}
	return nil
}

// testRevisionPending reports whether the run is waiting for the test role to
// answer an implementation objection. It is used by startup recovery to
// allow the next invocation in the same bounded cycle.
func testRevisionPending(run store.Run) bool {
	return run.Stage == store.StageTest && run.Status == store.StatusActive && run.TestObjection != nil
}

// implementationTestObjection reports whether a completed implementation
// handoff requests the protected test-stage objection route.
func implementationTestObjection(value report.Report, invocation store.Invocation) bool {
	return invocation.Role == workflow.RoleImplementation &&
		invocation.Stage == store.StageImplementation &&
		value.Outcome == report.OutcomeCompleted && value.Handoff != nil && len(value.Handoff.TestObjections) > 0
}

// validateImplementationTestObjection enforces the independent-test route and
// bounded-cycle preconditions before an implementation report can redirect a
// run away from implementation.
func validateImplementationTestObjection(value report.Report, invocation store.Invocation, run store.Run, packet SpecificationPacket) error {
	if !implementationTestObjection(value, invocation) {
		return nil
	}
	if !independentTestStageDeclared(packet) || run.TestStageSkipped {
		return errors.New("test objections require an active independent test stage")
	}
	if run.TestHandoff == nil || len(run.ProtectedTestPaths) == 0 {
		return errors.New("test objections require a completed protected test handoff")
	}
	budget := run.TestRevisionBudget
	if budget == 0 {
		budget = packet.RepositoryConfig.RetryLimits.TestRevision
	}
	if budget <= 0 {
		return errors.New("test objection revision budget must be positive")
	}
	if len(value.Handoff.TestObjections) != 1 {
		return errors.New("implementation must submit exactly one test objection per report")
	}
	return nil
}

// automatedTestObjectionGate reads the authorized measured-pilot decision
// before allowing an objection to start an automated revision. Configuration
// alone cannot open this gate; an authorized maintainer must have recorded the
// latest decision on issue #26.
func (s *Service) automatedTestObjectionGate(ctx context.Context, registration config.RepositoryRegistration, packet SpecificationPacket) (bool, string) {
	if !packet.RepositoryConfig.TestPolicy.AllowAutomatedObjections {
		return false, "test objection recorded; automated revision is disabled pending measured-pilot authorization"
	}
	if s.deps.Comments == nil {
		return false, "test objection recorded; issue #26 proceed decision could not be verified"
	}
	comments, err := s.deps.Comments.IssueComments(ctx, commandRepository(registration), measuredPilotIssueNumber)
	if err != nil {
		return false, "test objection recorded; issue #26 proceed decision could not be verified"
	}
	sort.SliceStable(comments, func(left, right int) bool {
		if !comments[left].UpdatedAt.IsZero() && !comments[right].UpdatedAt.IsZero() && !comments[left].UpdatedAt.Equal(comments[right].UpdatedAt) {
			return comments[left].UpdatedAt.Before(comments[right].UpdatedAt)
		}
		return compareCommentIDs(comments[left].ID, comments[right].ID) < 0
	})
	decision := ""
	for _, comment := range comments {
		if !authorizedCommentAuthor(registration.AuthorizedUsers, comment.Author) {
			continue
		}
		if value, ok := measuredPilotDecision(comment.Body); ok {
			decision = value
		}
	}
	if decision != "proceed" {
		return false, "test objection recorded; issue #26 has no authorized proceed decision"
	}
	return true, ""
}

// measuredPilotDecision extracts one exact decision line or machine-readable
// marker from a pilot comment. Ordinary prose mentioning the word proceed is
// intentionally not sufficient to authorize automation.
func measuredPilotDecision(body string) (string, bool) {
	const markerPrefix = "<!-- factory-pilot-decision:"
	const markerSuffix = "-->"
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		lowerLine := strings.ToLower(line)
		if strings.HasPrefix(lowerLine, markerPrefix) && strings.HasSuffix(lowerLine, markerSuffix) {
			value := strings.TrimSpace(line[len(markerPrefix) : len(line)-len(markerSuffix)])
			if decision, ok := normalizeMeasuredPilotDecision(value); ok {
				return decision, true
			}
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "-*# "))
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "decision") {
			continue
		}
		if decision, ok := normalizeMeasuredPilotDecision(parts[1]); ok {
			return decision, true
		}
	}
	return "", false
}

// normalizeMeasuredPilotDecision keeps the pilot decision vocabulary closed
// and strips only presentation backticks around a decision value.
func normalizeMeasuredPilotDecision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`")
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "proceed", "revise and repeat", "stop":
		return value, true
	default:
		return "", false
	}
}

// projectImplementationTestObjection moves a validated implementation
// objection to the test stage and freezes the dirty implementation paths that
// the resumed test role must treat as pre-existing context.
func projectImplementationTestObjection(previous store.Run, value report.Report, invocation store.Invocation, packet SpecificationPacket, observed gitadapter.WorktreeState, automated bool, automationReason string) (store.Run, error) {
	if err := validateImplementationTestObjection(value, invocation, previous, packet); err != nil {
		return store.Run{}, err
	}
	next := previous
	budget := next.TestRevisionBudget
	if budget == 0 {
		budget = packet.RepositoryConfig.RetryLimits.TestRevision
	}
	if budget <= 0 {
		return store.Run{}, errors.New("test objection revision budget must be positive")
	}
	next.TestRevisionBudget = budget
	if next.TestInvocationID == "" {
		return store.Run{}, errors.New("test objection has no original test invocation")
	}
	objection := value.Handoff.TestObjections[0]
	disputeKey := testObjectionDisputeKey(objection)
	if !testRevisionHistoryMatchesDispute(next.TestRevisionHistory, disputeKey) {
		next.TestRevisionAttempts = 0
		next.TestRevisionHistory = nil
	}
	next.TestObjection = &store.TestObjection{
		Test:         objection.Test,
		Claim:        objection.Claim,
		Evidence:     objection.Evidence,
		InvocationID: next.TestInvocationID,
	}
	basePaths, err := protectedTestPathsForCheckpoint(previous.Worktree, observed.ChangedPaths)
	if err != nil {
		return store.Run{}, fmt.Errorf("record test objection worktree context: %w", err)
	}
	next.TestRevisionBaseChangedPaths = basePaths
	next.RoleHandoff = roleHandoffFromReport(*value.Handoff)
	next.ImplementationHandoff = next.RoleHandoff
	next.PendingQuestions = nil
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	clearActiveInvocations(&next)
	next.TestStageSkipped = false
	if !automated {
		next.Stage = store.StageTest
		next.Status = store.StatusWaitingForHuman
		if strings.TrimSpace(automationReason) == "" {
			automationReason = "test objection recorded; automated revision is disabled pending measured-pilot authorization"
		}
		next.LifecycleReason = automationReason
		return next, nil
	}
	if next.TestRevisionAttempts >= budget {
		next.Stage = store.StageTest
		next.Status = store.StatusWaitingForHuman
		next.LifecycleReason = fmt.Sprintf("test objection submitted after %d revision attempts; waiting for human disposition", next.TestRevisionAttempts)
		return next, nil
	}
	next.TestRevisionAttempts++
	next.TestRevisionHistory = append(next.TestRevisionHistory, store.TestRevision{
		Attempt:    next.TestRevisionAttempts,
		Outcome:    store.TestRevisionPending,
		DisputeKey: disputeKey,
	})
	next.Stage = store.StageTest
	next.Status = store.StatusActive
	next.LifecycleReason = fmt.Sprintf("implementation test objection pending test revision %d/%d", next.TestRevisionAttempts, budget)
	return next, nil
}

// testObjectionDisputeKey identifies the behavioral claim under dispute while
// allowing updated evidence to accompany a later cycle for the same claim.
func testObjectionDisputeKey(objection report.TestObjection) string {
	value := strings.TrimSpace(objection.Test) + "\x00" + strings.TrimSpace(objection.Claim)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// testRevisionHistoryMatchesDispute reports whether retained cycles belong to
// the same test claim. Empty keys are legacy history and start a fresh dispute.
func testRevisionHistoryMatchesDispute(history []store.TestRevision, disputeKey string) bool {
	if len(history) == 0 || strings.TrimSpace(disputeKey) == "" {
		return len(history) == 0
	}
	for _, revision := range history {
		if revision.DisputeKey == "" || revision.DisputeKey != disputeKey {
			return false
		}
	}
	return true
}

// finishImplementationTestObjection starts the bounded test-role response or
// publishes the human pause when no revision budget remains.
func (s *Service) finishImplementationTestObjection(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		if err := s.stopRunWorker(ctx, run.ID); err != nil {
			return AgentResult{}, err
		}
	}
	if run.Status == store.StatusWaitingForHuman {
		if err := s.notifyWorkspace(ctx, registration, "factory test objection waiting", run.LifecycleReason); err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Invocation: *invocation, Report: value}, nil
	}
	if run.Status != store.StatusActive || run.Stage != store.StageTest {
		return AgentResult{}, fmt.Errorf("test objection did not enter an active test stage: %s/%s", run.Stage, run.Status)
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{
		RunID:        run.ID,
		Role:         workflow.RoleTest,
		Stage:        store.StageTest,
		testRevision: true,
	}); err != nil {
		return AgentResult{}, fmt.Errorf("resume original test session for objection: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// testRevisionChangedPaths returns paths newly changed after an implementation
// objection. Existing implementation changes remain in the worktree while the
// test role revises only its owned files.
func testRevisionChangedPaths(state gitadapter.WorktreeState, base []store.ProtectedTestPath) ([]string, error) {
	baseSet := make(map[string]struct{}, len(base))
	for _, path := range base {
		clean, err := cleanTestPath(path.Path)
		if err != nil {
			return nil, err
		}
		baseSet[clean] = struct{}{}
	}
	seen := make(map[string]struct{}, len(state.ChangedPaths))
	changed := make([]string, 0, len(state.ChangedPaths))
	for _, path := range state.ChangedPaths {
		clean, err := cleanTestPath(path)
		if err != nil {
			return nil, err
		}
		if _, alreadyDirty := baseSet[clean]; alreadyDirty {
			continue
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		changed = append(changed, clean)
	}
	sort.Strings(changed)
	return changed, nil
}

// validateTestRevisionBasePaths verifies that implementation changes present
// when an objection was submitted are byte-for-byte unchanged while the test
// role owns the resumed session.
func validateTestRevisionBasePaths(worktree string, base []store.ProtectedTestPath) error {
	for _, path := range base {
		current, err := testPathDigest(worktree, path.Path)
		if err != nil {
			return fmt.Errorf("inspect test-revision base path %q: %w", path.Path, err)
		}
		if current != path.SHA256 {
			return fmt.Errorf("test revision changed pre-existing path %q", path.Path)
		}
	}
	return nil
}

// mergeProtectedTestPaths refreshes hashes for the original protected tests
// and any files changed by an accepted revision.
func mergeProtectedTestPaths(worktree string, previous []store.ProtectedTestPath, changed []string) ([]store.ProtectedTestPath, error) {
	paths := make(map[string]struct{}, len(previous)+len(changed))
	for _, value := range previous {
		paths[value.Path] = struct{}{}
	}
	for _, path := range changed {
		clean, err := cleanTestPath(path)
		if err != nil {
			return nil, err
		}
		paths[clean] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return protectedTestPathsForCheckpoint(worktree, ordered)
}

// setTestRevisionOutcome replaces the pending history entry for an attempt,
// or appends one when recovering a legacy run without that entry.
func setTestRevisionOutcome(history []store.TestRevision, attempt int, outcome store.TestRevisionOutcome) []store.TestRevision {
	updated := append([]store.TestRevision(nil), history...)
	for index := range updated {
		if updated[index].Attempt == attempt {
			updated[index].Outcome = outcome
			return updated
		}
	}
	return append(updated, store.TestRevision{Attempt: attempt, Outcome: outcome})
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
		value.Role == workflow.RoleTest &&
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
	if run.TestObjection != nil {
		return s.acceptTestRevisionReport(ctx, registration, runStore, run, invocation, value, state)
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
		Role:              workflow.RoleTest,
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
	protected, err := protectedTestPathsForCheckpoint(run.Worktree, verifiedState.ChangedPaths)
	if err != nil {
		return AgentResult{}, err
	}
	checkpointRequest := gitadapter.CheckpointRequest{
		RunID:        run.ID,
		WorktreePath: run.Worktree,
		ParentSHA:    run.CheckpointSHA,
		Kind:         gitadapter.CheckpointKindTest,
		Paths:        append([]string(nil), verifiedState.ChangedPaths...),
		Message:      "verified focused test is red",
	}
	previous := *run
	handoff := convertTestHandoff(*value.TestHandoff)
	next := *run
	next.TestCheckpointSHA = ""
	next.SpecificationReview = nil
	next.StandardsReview = nil
	next.TestHandoff = &handoff
	next.ProtectedTestPaths = protected
	transition, transitionErr := workflow.DefaultRegistry().ResolveReportTransition(store.StageTest, report.OutcomeCompleted)
	if transitionErr != nil {
		return AgentResult{}, transitionErr
	}
	next.Stage = transition.Stage
	next.Status = transition.Status
	next.PendingQuestions = nil
	next.LifecycleReason = "test stage completed; protected handoff ready for implementation"
	next.Revision = previous.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	checkpoint := gitadapter.CheckpointResult{}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		repository := commandRepository(registration)
		issue, issueErr := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
		if issueErr != nil {
			return AgentResult{}, fmt.Errorf("read issue for test handoff checkpoint: %w", issueErr)
		}
		packet, packetErr := decodeSpecificationPacket(run.SpecificationPacket)
		if packetErr != nil {
			return AgentResult{}, fmt.Errorf("decode specification packet for test handoff checkpoint: %w", packetErr)
		}
		issue = ensureIssueIdentity(issue, packet.Issue, run.IssueNumber)
		checkpoint, next, err = s.checkpointAndPersistWithEffect(ctx, runStore, workspace, checkpointRequest, repository, issue, previous, next)
	} else {
		checkpoint, err = workspace.CreateCheckpoint(ctx, checkpointRequest)
	}
	if err != nil {
		return AgentResult{}, fmt.Errorf("create test checkpoint: %w", err)
	}
	if !github.ValidCommitSHA(checkpoint.SHA) {
		return AgentResult{}, errors.New("test checkpoint returned an invalid commit SHA")
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		*run = next
	} else {
		next.TestCheckpointSHA = checkpoint.SHA
		next.CheckpointSHA = checkpoint.SHA
		if err := s.persistAgentRunState(ctx, registration, runStore, previous, next); err != nil {
			return AgentResult{}, fmt.Errorf("persist test handoff: %w", err)
		}
		*run = next
	}
	nextRole, nextRoleDeclared := workflow.DefaultRegistry().RoleForRunStage(next.Stage)
	if !nextRoleDeclared {
		return AgentResult{}, fmt.Errorf("no role is declared for test handoff stage %q", next.Stage)
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{RunID: run.ID, Role: nextRole.Name, Stage: nextRole.Stage}); err != nil {
		return AgentResult{}, fmt.Errorf("launch next role after test handoff: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// acceptTestRevisionReport handles the test role's response to one
// implementation objection. Revisions are independently red-verified and
// checkpointed before the implementation role can resume.
func (s *Service) acceptTestRevisionReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report, state gitadapter.WorktreeState) (AgentResult, error) {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return AgentResult{}, fmt.Errorf("decode specification packet for test revision: %w", err)
	}
	if err := validateTestRevisionBasePaths(run.Worktree, run.TestRevisionBaseChangedPaths); err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, err.Error())
	}
	if value.Outcome == report.OutcomeNeedsClarification {
		previous := *run
		run.Status = store.StatusWaitingForHuman
		run.Stage = store.StageTest
		run.PendingQuestions = pendingQuestionsFromReport(value.Questions)
		run.ClarificationCommentID = ""
		run.ClarificationNotificationSent = false
		run.LifecycleReason = "test role requested clarification during objection revision"
		run.Revision = previous.Revision + 1
		run.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
			return AgentResult{}, fmt.Errorf("persist test objection clarification: %w", err)
		}
		published, err := s.ensureClarificationPublication(ctx, registration, runStore, *run)
		if err != nil {
			return AgentResult{}, err
		}
		*run = published
		return AgentResult{Invocation: *invocation, Report: value}, nil
	}
	if value.Outcome == report.OutcomeCannotProceed {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "test role cannot respond to implementation objection")
	}
	if value.TestObjectionResponse == nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "test objection response is missing")
	}
	newPaths, err := testRevisionChangedPaths(state, run.TestRevisionBaseChangedPaths)
	if err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test worktree paths could not be verified")
	}
	if err := validateTestChangedPaths(newPaths, packet.RepositoryConfig.TestPolicy); err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "test objection revision changed a non-test path")
	}
	if value.TestObjectionResponse.Decision == report.TestObjectionRejected {
		if len(newPaths) != 0 {
			return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "rejected test objection changed the worktree")
		}
		reason := "test role rejected implementation objection"
		if strings.TrimSpace(value.TestObjectionResponse.Reason) != "" {
			reason += ": " + value.TestObjectionResponse.Reason
		}
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionRejected, reason)
	}
	if value.TestObjectionResponse.Decision != report.TestObjectionAccepted {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "test objection response decision could not be verified")
	}
	if value.TestHandoff == nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "accepted test objection has no revised test handoff")
	}
	if len(newPaths) == 0 {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "accepted test objection did not revise a test path")
	}
	if s.deps.Worker == nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised red-test worker is unavailable")
	}
	result, commandErr := s.deps.Worker.RunCommand(ctx, worker.CommandRequest{
		RunID:             run.ID,
		Command:           value.TestHandoff.FocusedTestCommand,
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              workflow.RoleTest,
	})
	stopErr := s.stopRunWorker(ctx, run.ID)
	output := result.Stdout + "\n" + result.Stderr
	if commandErr != nil || stopErr != nil || result.ExitCode == 0 || !strings.Contains(output, value.TestHandoff.ExpectedFailureReason) {
		if stopErr != nil && commandErr == nil {
			commandErr = stopErr
		}
		reason := "revised focused red-test verification is disputed or unverifiable"
		if commandErr != nil {
			reason = "revised focused red-test verification could not be completed"
		} else if result.ExitCode != 0 && !strings.Contains(output, value.TestHandoff.ExpectedFailureReason) {
			reason = "revised focused red-test output did not contain the expected failure reason"
		}
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, reason)
	}
	inspector := s.worktreeInspector()
	if inspector == nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised red-test worktree inspection is unavailable")
	}
	verifiedState, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised red-test worktree inspection failed")
	}
	if verifiedState.HeadSHA != run.CheckpointSHA {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test-stage checkpoint changed during red-test verification")
	}
	if err := validateTestRevisionBasePaths(run.Worktree, run.TestRevisionBaseChangedPaths); err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised red-test changed pre-existing implementation work")
	}
	newPaths, err = testRevisionChangedPaths(verifiedState, run.TestRevisionBaseChangedPaths)
	if err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised red-test worktree paths could not be verified")
	}
	if err := validateTestChangedPaths(newPaths, packet.RepositoryConfig.TestPolicy); err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test-stage path ownership dispute")
	}
	if len(newPaths) == 0 {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test handoff has no changed test path")
	}
	protected, err := mergeProtectedTestPaths(run.Worktree, run.ProtectedTestPaths, newPaths)
	if err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised protected test paths could not be verified")
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test checkpoint workspace is unavailable")
	}
	checkpointRequest := gitadapter.CheckpointRequest{
		RunID:        run.ID,
		WorktreePath: run.Worktree,
		ParentSHA:    run.CheckpointSHA,
		Kind:         gitadapter.CheckpointKindTest,
		Paths:        append([]string(nil), newPaths...),
		Message:      "verified revised focused test is red",
	}
	previous := *run
	handoff := convertTestHandoff(*value.TestHandoff)
	next := *run
	next.TestHandoff = &handoff
	next.TestObjection = nil
	next.TestRevisionBaseChangedPaths = nil
	next.TestRevisionHistory = setTestRevisionOutcome(next.TestRevisionHistory, next.TestRevisionAttempts, store.TestRevisionAccepted)
	next.TestInvocationID = invocation.ID
	next.ProtectedTestPaths = protected
	next.SpecificationReview = nil
	next.StandardsReview = nil
	transition, transitionErr := workflow.DefaultRegistry().ResolveReportTransition(store.StageTest, report.OutcomeCompleted)
	if transitionErr != nil {
		return AgentResult{}, transitionErr
	}
	next.Stage = transition.Stage
	next.Status = transition.Status
	next.PendingQuestions = nil
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	next.LifecycleReason = fmt.Sprintf("test objection accepted; revised protected handoff ready for implementation (revision %d/%d)", next.TestRevisionAttempts, next.TestRevisionBudget)
	next.Revision = previous.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	checkpoint := gitadapter.CheckpointResult{}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		repository := commandRepository(registration)
		issue, issueErr := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
		if issueErr != nil {
			return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test checkpoint issue context could not be read")
		}
		issue = ensureIssueIdentity(issue, packet.Issue, run.IssueNumber)
		checkpoint, next, err = s.checkpointAndPersistWithEffect(ctx, runStore, workspace, checkpointRequest, repository, issue, previous, next)
	} else {
		checkpoint, err = workspace.CreateCheckpoint(ctx, checkpointRequest)
	}
	if err != nil {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test checkpoint could not be created")
	}
	if !github.ValidCommitSHA(checkpoint.SHA) {
		return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "revised test checkpoint returned an invalid commit identity")
	}
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		next.TestCheckpointSHA = checkpoint.SHA
		next.CheckpointSHA = checkpoint.SHA
		if err := s.persistAgentRunState(ctx, registration, runStore, previous, next); err != nil {
			return AgentResult{}, fmt.Errorf("persist revised test handoff: %w", err)
		}
	}
	*run = next
	nextRole, nextRoleDeclared := workflow.DefaultRegistry().RoleForRunStage(next.Stage)
	if !nextRoleDeclared {
		return AgentResult{}, fmt.Errorf("no role is declared for revised test handoff stage %q", next.Stage)
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{RunID: run.ID, Role: nextRole.Name, Stage: nextRole.Stage}); err != nil {
		return AgentResult{}, fmt.Errorf("launch implementation after revised test handoff: %w", err)
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// pauseTestRevisionForHuman records a bounded objection-cycle outcome and
// leaves the implementation changes available for explicit human review.
func (s *Service) pauseTestRevisionForHuman(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report, outcome store.TestRevisionOutcome, reason string) (AgentResult, error) {
	if err := s.stopRunWorker(ctx, run.ID); err != nil {
		return AgentResult{}, err
	}
	previous := *run
	run.Stage = store.StageTest
	run.Status = store.StatusWaitingForHuman
	clearActiveInvocations(run)
	run.PendingQuestions = nil
	run.TestRevisionHistory = setTestRevisionOutcome(run.TestRevisionHistory, run.TestRevisionAttempts, outcome)
	run.LifecycleReason = reason
	run.UpdatedAt = s.deps.Now().UTC()
	run.Revision = previous.Revision + 1
	if err := s.persistAgentRunState(ctx, registration, runStore, previous, *run); err != nil {
		return AgentResult{}, fmt.Errorf("persist test objection human pause: %w", err)
	}
	if err := s.notifyWorkspace(ctx, registration, "factory test objection waiting", run.ID+" is waiting for human test-objection disposition"); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Invocation: *invocation, Report: value}, nil
}

// pauseUnverifiableTestRevisionReport converts a structurally invalid but
// identity-bound revision report into a durable human pause before the normal
// result-acceptance effect can mark its invocation complete.
func (s *Service) pauseUnverifiableTestRevisionReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if err := s.stopRunWorker(ctx, run.ID); err != nil {
		return AgentResult{}, err
	}
	if invocation.NativeSessionID != "" {
		_, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(invocation.Harness))
		if err != nil {
			return AgentResult{}, fmt.Errorf("ensure agent runtime for unverifiable test revision: %w", err)
		}
		if err := harnessRuntime.Finish(ctx, harness.Session{
			InvocationID:    invocation.ID,
			NativeSessionID: invocation.NativeSessionID,
			Surface:         invocationSurface(*invocation),
		}); err != nil {
			return AgentResult{}, fmt.Errorf("finish unverifiable test revision session: %w", err)
		}
	}
	invocation.Status = store.InvocationStatusWaitingForHuman
	invocation.UpdatedAt = s.deps.Now().UTC()
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return AgentResult{}, errors.New("operational store does not support test revision invocation persistence")
	}
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return AgentResult{}, fmt.Errorf("persist unverifiable test revision invocation: %w", err)
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationTestDispute); err != nil {
			return AgentResult{}, fmt.Errorf("record test objection verification dispute: %w", err)
		}
	}
	return s.pauseTestRevisionForHuman(ctx, registration, runStore, run, invocation, value, store.TestRevisionVerificationFailed, "test objection revision report is unverifiable")
}

// pauseUnverifiableTestReport stops the visible test execution before placing
// a structurally invalid completed test report in human disposition.
func (s *Service) pauseUnverifiableTestReport(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, invocation *store.Invocation, value report.Report) (AgentResult, error) {
	if err := s.stopRunWorker(ctx, run.ID); err != nil {
		return AgentResult{}, err
	}
	if invocation.NativeSessionID != "" {
		_, harnessRuntime, err := s.ensureAgentRuntime(registration.Cmux.SocketPath, config.Harness(invocation.Harness))
		if err != nil {
			return AgentResult{}, fmt.Errorf("ensure agent runtime for unverifiable test report: %w", err)
		}
		if err := harnessRuntime.Finish(ctx, harness.Session{
			InvocationID:    invocation.ID,
			NativeSessionID: invocation.NativeSessionID,
			Surface:         invocationSurface(*invocation),
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
	transition, err := workflow.DefaultRegistry().ResolveReportTransition(invocation.Stage, value.Outcome)
	if err != nil {
		return AgentResult{}, fmt.Errorf("resolve technical test exemption transition: %w", err)
	}
	nextRole, nextRoleDeclared := workflow.DefaultRegistry().RoleForRunStage(transition.Stage)
	if !nextRoleDeclared {
		return AgentResult{}, fmt.Errorf("no role is declared for technical test exemption stage %q", transition.Stage)
	}
	previous := *run
	run.Stage = transition.Stage
	run.Status = transition.Status
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
	if _, err := s.startAgentWithStore(ctx, registration, runStore, run, AgentRequest{RunID: run.ID, Role: nextRole.Name, Stage: nextRole.Stage}); err != nil {
		return AgentResult{}, fmt.Errorf("launch next role after technical exemption: %w", err)
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
