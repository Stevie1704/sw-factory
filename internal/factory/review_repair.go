package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// LatestInvocationByRoleStore is the optional restart-safe lookup used to
// resume the implementation session that produced the reviewed checkpoint.
type LatestInvocationByRoleStore interface {
	LatestInvocationByRole(context.Context, string, string) (*store.Invocation, error)
}

// ReviewRepairResult reports the bounded review-repair transition and any
// implementation invocation launched for it.
type ReviewRepairResult struct {
	// Run is the durable run projection after routing.
	Run store.Run
	// Outcome identifies whether repair started or human disposition is needed.
	Outcome store.ReviewRepairOutcome
	// Attempt is the current or newly reserved repair round.
	Attempt int
	// Budget is the frozen repair ceiling.
	Budget int
	// Remaining is the unused budget after a start decision.
	Remaining int
	// Invocation is populated when a visible implementation repair launched.
	Invocation *store.Invocation
}

// reviewRepairDecisionKind identifies the coordinator action for one complete
// blocking-review packet.
type reviewRepairDecisionKind string

const (
	// reviewRepairDecisionStart permits one bounded implementation repair.
	reviewRepairDecisionStart reviewRepairDecisionKind = "start"
	// reviewRepairDecisionEscalate preserves the blocker for human disposition.
	reviewRepairDecisionEscalate reviewRepairDecisionKind = "escalate"
	// reviewRepairDecisionWait preserves an unresolved reservation or disabled
	// policy without attempting another visible edit.
	reviewRepairDecisionWait reviewRepairDecisionKind = "wait"
)

// reviewRepairDecision is the content-free result of the review-repair
// workflow table. Finding text remains in the packet; this value carries only
// the bounded transition and opaque blocker identities.
type reviewRepairDecision struct {
	// Kind identifies the next coordinator action.
	Kind reviewRepairDecisionKind
	// Outcome identifies the durable lifecycle result, when one applies.
	Outcome store.ReviewRepairOutcome
	// Attempt identifies the repair round associated with the decision.
	Attempt int
	// Remaining is the number of unused rounds after a start decision.
	Remaining int
	// BlockerKeys are opaque identities used for repeated-blocker detection.
	BlockerKeys []string
}

// decideReviewRepair applies the bounded review-repair policy to one complete
// packet. A repeated blocker escalates immediately, even when budget remains.
func decideReviewRepair(run store.Run, findings []store.ReviewRepairFinding) reviewRepairDecision {
	keys := reviewRepairBlockerKeys(findings)
	decision := reviewRepairDecision{BlockerKeys: keys}
	if len(keys) == 0 || run.ReviewRepairBudget <= 0 || run.ReviewRepairPendingAttempt != 0 {
		decision.Kind = reviewRepairDecisionWait
		decision.Outcome = store.ReviewRepairWaitingForHuman
		return decision
	}
	if reviewRepairHasRepeatedBlocker(run.ReviewRepairHistory, keys) {
		decision.Kind = reviewRepairDecisionEscalate
		decision.Outcome = store.ReviewRepairRepeated
		decision.Attempt = run.ReviewRepairAttempts
		decision.Remaining = reviewRepairRemaining(run.ReviewRepairBudget, run.ReviewRepairAttempts)
		return decision
	}
	if run.ReviewRepairAttempts >= run.ReviewRepairBudget {
		decision.Kind = reviewRepairDecisionEscalate
		decision.Outcome = store.ReviewRepairExhausted
		decision.Attempt = run.ReviewRepairAttempts
		return decision
	}
	decision.Kind = reviewRepairDecisionStart
	decision.Outcome = store.ReviewRepairStarted
	decision.Attempt = run.ReviewRepairAttempts + 1
	decision.Remaining = reviewRepairRemaining(run.ReviewRepairBudget, decision.Attempt)
	return decision
}

// reviewRepairRemaining returns a non-negative budget remainder.
func reviewRepairRemaining(budget, attempts int) int {
	if budget <= attempts {
		return 0
	}
	return budget - attempts
}

// reviewRepairBlockerKeys computes stable opaque identities for a combined
// reviewer packet without retaining reviewer prose in evaluation projections.
func reviewRepairBlockerKeys(findings []store.ReviewRepairFinding) []string {
	keys := make([]string, 0, len(findings))
	seen := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		key := reviewRepairFindingKey(finding)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// reviewRepairFindingKey hashes stable finding identity fields. Reviewer role
// is retained in the repair packet for provenance but excluded here so the
// same material blocker is recognized even when the other reviewer reports it
// on the next round. Line movement and common claim paraphrases are normalized
// while a distinct claim in the same file/category remains distinguishable.
func reviewRepairFindingKey(finding store.ReviewRepairFinding) string {
	value := strings.Join([]string{
		reviewRepairLocationKey(finding.Finding.Location),
		reviewRepairKeyValue(finding.Finding.Category),
		reviewRepairClaimKeyValue(finding.Finding.Claim),
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// reviewRepairLocationKey reduces a source location to its stable ownership
// surface. Line and column movement between repair rounds, or a paraphrased
// claim from a fresh reviewer session, must not let the same file-level
// blocker consume a second automatic repair attempt.
func reviewRepairLocationKey(value string) string {
	value = reviewRepairKeyValue(value)
	if marker := strings.LastIndex(value, "#l"); marker >= 0 && allDecimal(value[marker+2:]) {
		value = strings.TrimSpace(value[:marker])
	}
	parts := strings.Split(value, ":")
	for len(parts) > 1 && allDecimal(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, ":")
}

// allDecimal reports whether a location suffix is a line or column number.
func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, runeValue := range value {
		if runeValue < '0' || runeValue > '9' {
			return false
		}
	}
	return true
}

// reviewRepairKeyValue canonicalizes bounded reviewer metadata before hashing.
func reviewRepairKeyValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(reviewSingleLine(value)), " "))
}

// reviewRepairClaimKeyValue canonicalizes only safe claim vocabulary before
// repeated-blocker hashing. It preserves negation, word order, and material
// nouns so opposite or merely related claims cannot collapse into one key.
func reviewRepairClaimKeyValue(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(reviewSingleLine(value)), " "))
	for _, replacement := range []struct {
		old string
		new string
	}{
		{old: "doesn't verify", new: "skips"},
		{old: "does not verify", new: "skips"},
		{old: "doesn't check", new: "skips"},
		{old: "does not check", new: "skips"},
		{old: "doesn't validate", new: "skips"},
		{old: "does not validate", new: "skips"},
	} {
		value = strings.ReplaceAll(value, replacement.old, replacement.new)
	}
	stopWords := map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "be": {}, "before": {}, "by": {},
		"can": {}, "for": {}, "from": {}, "has": {}, "have": {}, "in": {}, "into": {},
		"is": {}, "it": {}, "must": {}, "of": {}, "on": {}, "should": {}, "that": {},
		"the": {}, "this": {}, "to": {}, "was": {}, "were": {}, "with": {},
	}
	synonyms := map[string]string{
		"readiness": "ready", "ready": "ready", "last": "final", "final": "final",
		"skip": "skip", "skips": "skip", "omitted": "skip", "omits": "skip", "omit": "skip",
		"check": "check", "checks": "check", "checked": "check", "checking": "check",
		"validate": "validate", "validates": "validate", "validated": "validate", "validation": "validate",
		"verify": "verify", "verifies": "verify", "verified": "verify", "verification": "verify",
	}
	words := strings.Fields(value)
	canonical := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.Trim(word, "`.,:;!?()[]{}\"")
		if word == "" {
			continue
		}
		if _, stop := stopWords[word]; stop {
			continue
		}
		if replacement, ok := synonyms[word]; ok {
			word = replacement
		}
		if word == "transition" || word == "transitions" {
			continue
		}
		canonical = append(canonical, word)
	}
	return strings.Join(canonical, " ")
}

// reviewRepairTestOwnerFindingKey hashes the stable identity used to remember
// which test-owned finding was accepted through the objection protocol.
func reviewRepairTestOwnerFindingKey(finding store.ReviewRepairFinding) string {
	value := strings.Join([]string{
		reviewRepairKeyValue(finding.Finding.Location),
		reviewRepairKeyValue(finding.Finding.Claim),
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// reviewRepairTestOwnerFindingMatches reports whether one structured test
// objection identifies a test-owned review finding by claim or test location.
func reviewRepairTestOwnerFindingMatches(test, claim string, finding store.ReviewRepairFinding) bool {
	if reviewRepairKeyValue(claim) != "" && reviewRepairKeyValue(claim) == reviewRepairKeyValue(finding.Finding.Claim) {
		return true
	}
	return reviewRepairLocationKey(test) != "" && reviewRepairLocationKey(test) == reviewRepairLocationKey(finding.Finding.Location)
}

// reviewRepairTestOwnerFindingResolved reports whether the test-owned finding
// was already accepted and revised during the current repair packet.
func reviewRepairTestOwnerFindingResolved(packet store.ReviewRepairPacket, finding store.ReviewRepairFinding) bool {
	key := reviewRepairTestOwnerFindingKey(finding)
	for _, resolved := range packet.ResolvedTestOwnerFindingKeys {
		if resolved == key {
			return true
		}
	}
	return false
}

// unresolvedReviewRepairTestOwnerFindings returns only test-owned findings
// whose objection cycle has not yet produced an accepted revised test.
func unresolvedReviewRepairTestOwnerFindings(packet store.ReviewRepairPacket) []store.ReviewRepairFinding {
	findings := make([]store.ReviewRepairFinding, 0)
	for _, finding := range packet.Findings {
		if strings.EqualFold(strings.TrimSpace(finding.Finding.SuggestedOwner), workflow.RoleTest) && !reviewRepairTestOwnerFindingResolved(packet, finding) {
			findings = append(findings, finding)
		}
	}
	return findings
}

// recordReviewRepairTestOwnerResolution records an accepted independent-test
// objection without mutating the prior packet through a shared pointer.
func recordReviewRepairTestOwnerResolution(run *store.Run) error {
	if run == nil || run.ReviewRepairPacket == nil || run.TestObjection == nil {
		return nil
	}
	packet := *run.ReviewRepairPacket
	for _, finding := range unresolvedReviewRepairTestOwnerFindings(packet) {
		if !reviewRepairTestOwnerFindingMatches(run.TestObjection.Test, run.TestObjection.Claim, finding) {
			continue
		}
		key := reviewRepairTestOwnerFindingKey(finding)
		for _, resolved := range packet.ResolvedTestOwnerFindingKeys {
			if resolved == key {
				run.ReviewRepairPacket = &packet
				return nil
			}
		}
		packet.ResolvedTestOwnerFindingKeys = append(packet.ResolvedTestOwnerFindingKeys, key)
		run.ReviewRepairPacket = &packet
		return nil
	}
	return errors.New("accepted test objection does not identify an unresolved test-owner review finding")
}

// validateReviewRepairTestOwnerRouting requires a review-repair report to use
// the existing structured objection protocol whenever a reviewer assigns a
// blocker to the test role. The coordinator therefore never accepts a direct
// implementation handoff that silently edits or dismisses protected tests.
func validateReviewRepairTestOwnerRouting(value report.Report, invocation store.Invocation, run store.Run, packet SpecificationPacket) error {
	if invocation.Role != workflow.RoleImplementation || invocation.Stage != store.StageImplementation || value.Outcome != report.OutcomeCompleted || value.Handoff == nil || run.ReviewRepairPacket == nil {
		return nil
	}
	testOwnerFindings := unresolvedReviewRepairTestOwnerFindings(*run.ReviewRepairPacket)
	if len(testOwnerFindings) == 0 {
		return nil
	}
	if !independentTestStageDeclared(packet) || run.TestStageSkipped {
		return errors.New("review repair assigned to the test role requires an active independent test stage")
	}
	if len(value.Handoff.TestObjections) == 0 {
		return errors.New("review repair assigned to the test role requires a structured test objection")
	}
	if len(value.Handoff.TestObjections) != 1 {
		return errors.New("review repair must submit exactly one structured test objection per implementation report")
	}
	objection := value.Handoff.TestObjections[0]
	for _, finding := range testOwnerFindings {
		if reviewRepairTestOwnerFindingMatches(objection.Test, objection.Claim, finding) {
			return nil
		}
	}
	return errors.New("structured test objection must identify one unresolved test-owner review finding")
}

// reviewRepairHasRepeatedBlocker reports whether any current blocker identity
// occurred in a prior repair round.
func reviewRepairHasRepeatedBlocker(history []store.ReviewRepairAttempt, keys []string) bool {
	seen := make(map[string]struct{}, len(history))
	for _, attempt := range history {
		for _, key := range attempt.BlockerKeys {
			seen[key] = struct{}{}
		}
	}
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			return true
		}
	}
	return false
}

// reviewRepairHistoryWithOutcome records one bounded lifecycle transition and
// preserves opaque blocker identities for future repeated-finding detection.
func reviewRepairHistoryWithOutcome(history []store.ReviewRepairAttempt, attempt int, outcome store.ReviewRepairOutcome, packet *store.ReviewRepairPacket) []store.ReviewRepairAttempt {
	if attempt <= 0 {
		return append([]store.ReviewRepairAttempt(nil), history...)
	}
	keys := []string(nil)
	if packet != nil {
		keys = reviewRepairBlockerKeys(packet.Findings)
	}
	updated := append([]store.ReviewRepairAttempt(nil), history...)
	for index := range updated {
		if updated[index].Attempt != attempt {
			continue
		}
		updated[index].Outcome = outcome
		if len(keys) > 0 {
			updated[index].BlockerKeys = mergeReviewRepairKeys(updated[index].BlockerKeys, keys)
		}
		return updated
	}
	updated = append(updated, store.ReviewRepairAttempt{Attempt: attempt, Outcome: outcome, BlockerKeys: keys})
	return updated
}

// mergeReviewRepairKeys appends only unseen opaque blocker identities.
func mergeReviewRepairKeys(existing, additions []string) []string {
	result := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(result)+len(additions))
	for _, key := range result {
		seen[key] = struct{}{}
	}
	for _, key := range additions {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

// latestImplementationInvocation finds the newest implementation session
// without confusing a more recent isolated reviewer invocation for the repair
// source. A store that lacks the role-specific projection starts a fresh
// implementation session rather than guessing from mixed role history.
func latestImplementationInvocation(ctx context.Context, runStore RunStore, runID string) (*store.Invocation, error) {
	if roleStore, ok := runStore.(LatestInvocationByRoleStore); ok {
		invocation, err := roleStore.LatestInvocationByRole(ctx, runID, workflow.RoleImplementation)
		if err != nil {
			return nil, fmt.Errorf("look up latest implementation invocation: %w", err)
		}
		return invocation, nil
	}
	return nil, nil
}

// blockingReviewFindingsForRun combines every exact-checkpoint blocking
// finding in reviewer declaration order while retaining its isolated owner.
func blockingReviewFindingsForRun(run store.Run) []store.ReviewRepairFinding {
	results := []struct {
		role   string
		result *store.ReviewResult
	}{
		{role: workflow.RoleSpecificationReview, result: run.SpecificationReview},
		{role: workflow.RoleStandardsReview, result: run.StandardsReview},
	}
	findings := make([]store.ReviewRepairFinding, 0)
	for _, value := range results {
		if !reviewRoleConfigured(run, value.role) || value.result == nil || value.result.CheckpointSHA != run.CheckpointSHA {
			continue
		}
		for _, finding := range value.result.Findings {
			if reviewFindingBlocks(finding) {
				findings = append(findings, store.ReviewRepairFinding{ReviewerRole: value.role, Finding: finding})
			}
		}
	}
	return findings
}

// reviewRepairPacketForDecision constructs the coordinator-owned packet sent
// to implementation after the review round has completed.
func reviewRepairPacketForDecision(run store.Run, findings []store.ReviewRepairFinding, decision reviewRepairDecision) *store.ReviewRepairPacket {
	if len(findings) == 0 || decision.Attempt <= 0 || run.ReviewRepairBudget <= 0 {
		return nil
	}
	return &store.ReviewRepairPacket{
		Version:       1,
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Attempt:       decision.Attempt,
		Budget:        run.ReviewRepairBudget,
		Findings:      append([]store.ReviewRepairFinding(nil), findings...),
	}
}

// recordReviewRepairEvaluation records only bounded workflow categories. No
// reviewer finding text or blocker identity enters the evaluation summary.
func recordReviewRepairEvaluation(ctx context.Context, runStore RunStore, run store.Run, decision reviewRepairDecision) error {
	recorder, ok := runStore.(evaluationRecorder)
	if !ok {
		return nil
	}
	if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
		return err
	}
	if err := recorder.RecordEvaluationBlocker(ctx, run.ID, store.EvaluationBlockerReview); err != nil {
		return err
	}
	if decision.Kind == reviewRepairDecisionStart {
		if err := recorder.RecordEvaluationAttempt(ctx, run.ID, store.EvaluationAttemptReviewRevision); err != nil {
			return err
		}
	}
	if decision.Kind == reviewRepairDecisionEscalate || decision.Kind == reviewRepairDecisionWait {
		if err := recorder.RecordEvaluationEscalation(ctx, run.ID, store.EvaluationEscalationReviewFinding); err != nil {
			return err
		}
	}
	if decision.Outcome == store.ReviewRepairExhausted {
		if err := recorder.RecordEvaluationBudgetExhaustion(ctx, run.ID); err != nil {
			return err
		}
	}
	return nil
}

// routeReviewRepair applies the review-repair decision after all configured
// reviewers have reported the same checkpoint. It routes every finding to one
// implementation packet; the implementation role uses the existing test
// objection machinery when a finding names test ownership.
func (s *Service) routeReviewRepair(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (ReviewRepairResult, error) {
	findings := blockingReviewFindingsForRun(run)
	if run.ReviewRepairBudget == 0 {
		packet, err := decodeSpecificationPacket(run.SpecificationPacket)
		if err != nil {
			return ReviewRepairResult{Run: run}, fmt.Errorf("decode specification packet for review repair: %w", err)
		}
		run.ReviewRepairBudget = packet.RepositoryConfig.RetryLimits.ReviewRepair
	}
	decision := decideReviewRepair(run, findings)
	result := ReviewRepairResult{
		Run:       run,
		Outcome:   decision.Outcome,
		Attempt:   decision.Attempt,
		Budget:    run.ReviewRepairBudget,
		Remaining: decision.Remaining,
	}
	if err := recordReviewRepairEvaluation(ctx, runStore, run, decision); err != nil {
		return result, fmt.Errorf("record review-repair evaluation: %w", err)
	}

	switch decision.Kind {
	case reviewRepairDecisionStart:
		packet := reviewRepairPacketForDecision(run, findings, decision)
		if packet == nil {
			return result, errors.New("review repair start requires a blocking finding packet")
		}
		next := run
		next.Stage = store.StageImplementation
		next.Status = store.StatusActive
		next.LifecycleReason = fmt.Sprintf("blocking review findings routed to implementation repair attempt %d of %d", decision.Attempt, run.ReviewRepairBudget)
		next.ReviewRepairAttempts = decision.Attempt
		next.ReviewRepairPendingAttempt = decision.Attempt
		next.ReviewRepairPacket = packet
		next.ReviewRepairHistory = reviewRepairHistoryWithOutcome(run.ReviewRepairHistory, decision.Attempt, store.ReviewRepairPending, packet)
		next.PendingQuestions = nil
		next.ClarificationCommentID = ""
		next.ClarificationNotificationSent = false
		next.UpdatedAt = s.deps.Now().UTC()
		if _, ok := runStore.(InvocationStore); !ok {
			next.ReviewRepairPendingAttempt = 0
			next.Stage = store.StageReview
			next.Status = store.StatusWaitingForHuman
			next.LifecycleReason = "review repair requires a visible implementation invocation"
			next.ReviewRepairHistory = reviewRepairHistoryWithOutcome(next.ReviewRepairHistory, decision.Attempt, store.ReviewRepairWaitingForHuman, packet)
			next.UpdatedAt = s.deps.Now().UTC()
			if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
				return result, fmt.Errorf("persist unavailable review-repair route: %w", err)
			}
			result.Run = next
			result.Outcome = store.ReviewRepairWaitingForHuman
			return result, nil
		}
		if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
			return result, fmt.Errorf("persist review-repair reservation: %w", err)
		}
		launch, err := s.startAgentWithStore(ctx, registration, runStore, &next, AgentRequest{
			RunID: run.ID, Role: workflow.RoleImplementation, Stage: store.StageImplementation, reviewRepair: true,
		})
		if err != nil {
			if journal, journaled := runStore.(PendingEffectStore); journaled {
				pending, pendingErr := journal.PendingEffect(context.WithoutCancel(ctx), run.ID)
				if pendingErr == nil && pending != nil {
					result.Run = next
					result.Outcome = store.ReviewRepairPending
					return result, fmt.Errorf("start review-repair implementation: %w", err)
				}
			}
			if harness.IsRateLimited(err) || harness.IsAuthenticationExpired(err) || harness.IsUnexpectedExit(err) {
				if current, currentErr := runStore.CurrentRun(ctx); currentErr == nil && current != nil {
					result.Run = *current
				}
				return result, fmt.Errorf("start review-repair implementation: %w", err)
			}
			failed := next
			failed.ReviewRepairPendingAttempt = 0
			failed.Stage = store.StageReview
			failed.Status = store.StatusWaitingForHuman
			failed.LifecycleReason = "review repair could not start; human disposition required"
			failed.ReviewRepairHistory = reviewRepairHistoryWithOutcome(failed.ReviewRepairHistory, decision.Attempt, store.ReviewRepairWaitingForHuman, packet)
			failed.UpdatedAt = s.deps.Now().UTC()
			transitionErr := s.persistAgentRunState(ctx, registration, runStore, next, failed)
			result.Run = failed
			result.Outcome = store.ReviewRepairWaitingForHuman
			return result, errors.Join(fmt.Errorf("start review-repair implementation: %w", err), transitionErr)
		}
		result.Run = next
		result.Invocation = &launch.Invocation
		if current, currentErr := runStore.CurrentRun(ctx); currentErr == nil && current != nil {
			result.Run = *current
		}
		return result, nil

	case reviewRepairDecisionEscalate, reviewRepairDecisionWait:
		next := run
		next.Stage = store.StageReview
		next.Status = store.StatusWaitingForHuman
		next.LifecycleReason = reviewRepairHumanReason(decision.Outcome)
		if packet := reviewRepairPacketForDecision(run, findings, decision); packet != nil {
			next.ReviewRepairPacket = packet
			next.ReviewRepairHistory = reviewRepairHistoryWithOutcome(run.ReviewRepairHistory, decision.Attempt, decision.Outcome, packet)
		}
		next.ReviewRepairPendingAttempt = 0
		next.UpdatedAt = s.deps.Now().UTC()
		if err := s.persistAgentRunState(ctx, registration, runStore, run, next); err != nil {
			return result, fmt.Errorf("persist review-repair escalation: %w", err)
		}
		result.Run = next
		return result, nil
	default:
		return result, fmt.Errorf("unsupported review-repair decision %q", decision.Kind)
	}
}

// reviewRepairHumanReason returns bounded operator-facing reasons for a
// review-repair decision that cannot safely launch another edit.
func reviewRepairHumanReason(outcome store.ReviewRepairOutcome) string {
	switch outcome {
	case store.ReviewRepairRepeated:
		return "materially repeated review blocker; human disposition required"
	case store.ReviewRepairExhausted:
		return "review-repair budget exhausted; human disposition required"
	default:
		return "review repair cannot continue automatically; human disposition required"
	}
}
