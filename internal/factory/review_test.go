package factory_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const reviewCheckpoint = "fedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedc"

// TestSpecificationReviewUsesAnImmutablePacketAndRoutesAdvisories verifies
// the independent reviewer receives the exact artifact identity without
// inheriting implementation, test, or other-reviewer session context.
func TestSpecificationReviewUsesAnImmutablePacketAndRoutesAdvisories(t *testing.T) {
	fixture := newReviewFixture(t)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.Role != "spec_review" || launch.Invocation.Stage != store.StageReview {
		t.Fatalf("review invocation = %#v, want spec_review/review", launch.Invocation)
	}
	if len(fixture.statuses.values) != 1 || fixture.statuses.values[0].State != github.CommitStatusPending || fixture.statuses.values[0].SHA != reviewCheckpoint || fixture.statuses.values[0].Context != factory.SpecificationReviewStatusContext {
		t.Fatalf("review statuses after launch = %#v, want pending exact-checkpoint status", fixture.statuses.values)
	}
	if len(fixture.worker.starts) != 1 || len(fixture.worker.commands) != 0 {
		t.Fatalf("review worker effects = starts %#v commands %#v, want one start and no diff command", fixture.worker.starts, fixture.worker.commands)
	}
	// The prompt names the diff's location instead of carrying a second copy,
	// so its size does not follow the reviewed change.
	reviewPrompt := fixture.harness.starts[len(fixture.harness.starts)-1].Prompt
	if strings.Contains(reviewPrompt, "diff --git") {
		t.Fatalf("review prompt repeats the mounted diff:\n%s", reviewPrompt)
	}
	if !strings.Contains(reviewPrompt, prompt.WorkerReviewDiffPath) || !strings.Contains(reviewPrompt, "sed -n '1,200p'") {
		t.Fatalf("review prompt does not name the mounted diff:\n%s", reviewPrompt)
	}
	if !fixture.worker.starts[0].WorktreeReadOnly {
		t.Fatal("review worker worktree is writable, want a read-only checkpoint mount")
	}
	packetData, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read review packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(packetData, &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	diff := fixture.worktree.diff
	digest := sha256.Sum256([]byte(diff))
	if packet.ReviewContext == nil || packet.ReviewContext.CheckpointSHA != reviewCheckpoint || packet.ReviewContext.DiffPath != prompt.WorkerReviewDiffPath || packet.ReviewContext.DiffBytes != int64(len(diff)) || packet.ReviewContext.DiffSHA256 != hex.EncodeToString(digest[:]) || packet.ReviewContext.CurrentDiff != "" || packet.ReviewContext.OmittedDiffBytes != 0 || packet.ReviewContext.ChangedPathsCommand != "" || packet.ReviewContext.DiffPathCommand != "" || packet.ReviewContext.ImplementationHandoff != nil || packet.ReviewContext.TestHandoff != nil || len(packet.ReviewContext.ProtectedTestPaths) != 0 || packet.ReviewContext.TestExemption != nil {
		t.Fatalf("review context = %#v, want artifact identity without historical or implementation/test context", packet.ReviewContext)
	}
	artifactPath := filepath.Join(launch.Invocation.InvocationDirectory, "review.diff")
	artifactInfo, err := os.Lstat(artifactPath)
	if err != nil {
		t.Fatalf("stat review artifact: %v", err)
	}
	if !artifactInfo.Mode().IsRegular() || artifactInfo.Size() != int64(len(diff)) {
		t.Fatalf("review artifact info = %#v, want regular file of %d bytes", artifactInfo, len(diff))
	}
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read review artifact: %v", err)
	}
	if string(artifact) != diff {
		t.Fatalf("review artifact = %q, want %q", artifact, diff)
	}
	if packet.TestHandoff != nil || packet.TestObjection != nil || packet.TestExemption != nil || len(packet.ProtectedTestPaths) != 0 {
		t.Fatalf("review packet inherited test context: %#v", packet)
	}
	for _, marker := range []string{workflow.PromptVersionSpecificationReview, "exact checkpoint", "--finding", "no upstream harness transcript"} {
		if !strings.Contains(strings.ToLower(launch.Prompt), strings.ToLower(marker)) {
			t.Errorf("review prompt missing %q:\n%s", marker, launch.Prompt)
		}
	}

	reportValue := reviewReport(launch, []report.ReviewFinding{
		{Location: "internal/factory/review.go:20", Claim: "scope could be narrower", Evidence: "the helper is reusable", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryScope, SuggestedResolution: "keep the helper", SuggestedOwner: "implementation"},
		{Location: "internal/factory/review.go:21", Claim: "name is subjective", Evidence: "no behavior changes", Severity: report.ReviewSeverityAdvisory, Category: report.ReviewCategoryTaste, SuggestedResolution: "consider a shorter name", SuggestedOwner: "implementation"},
	})
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	accepted, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Report.ReviewHandoff == nil || fixture.runStore.current.Stage != store.StageReady || fixture.runStore.current.Status != store.StatusActive {
		t.Fatalf("accepted review = %#v run=%#v, want ready/active", accepted.Report, fixture.runStore.current)
	}
	if fixture.pullRequests.existing.Draft {
		t.Fatal("ready pull request remained draft")
	}
	if !fixture.runStore.current.ReadyNotificationSent || len(fixture.runStore.current.ActiveInvocationIDs) != 0 {
		t.Fatalf("ready projection = %#v, want durable notification and no active invocation", fixture.runStore.current)
	}
	if len(fixture.statuses.values) != 2 || fixture.statuses.values[1].State != github.CommitStatusSuccess {
		t.Fatalf("review statuses after acceptance = %#v, want pending then success", fixture.statuses.values)
	}
	body := fixture.pullRequests.updatedRequests[len(fixture.pullRequests.updatedRequests)-1].Body
	for _, marker := range []string{"Specification review", "scope", "taste", "suggested owner", "keep the helper"} {
		if !strings.Contains(body, marker) {
			t.Errorf("PR review projection missing %q: %s", marker, body)
		}
	}
}

// TestSpecificationReviewRefusesReadinessWithoutFinalCheckpointGates
// verifies a blocker-free review cannot mark a pull request ready when the
// final checkpoint gate projection is missing success.
// TestSpecificationReviewStreamsAnOversizedDiff verifies a checkpoint whose
// diff exceeds the old packet bound still reaches a reviewer with the complete
// artifact and bounded packet metadata.
func TestSpecificationReviewStreamsAnOversizedDiff(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.worktree.diff = strings.Repeat("-\tremoved line\n", 20000)

	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v, want an oversized diff to launch a reviewer", err)
	}
	if len(fixture.harness.starts) != 1 {
		t.Fatalf("harness starts = %#v, want one reviewer launched for an oversized diff", fixture.harness.starts)
	}
	if strings.Contains(launch.Prompt, "removed line") {
		t.Error("review prompt repeated the mounted diff")
	}
	for _, marker := range []string{prompt.WorkerReviewDiffPath, "sed -n '1,200p'", "sed -n '201,400p'"} {
		if !strings.Contains(launch.Prompt, marker) {
			t.Errorf("review prompt missing %q: %s", marker, launch.Prompt)
		}
	}
	packetData, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read invocation packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(packetData, &packet); err != nil {
		t.Fatalf("decode invocation packet: %v", err)
	}
	diff := fixture.worktree.diff
	digest := sha256.Sum256([]byte(diff))
	if packet.ReviewContext == nil || packet.ReviewContext.DiffPath != prompt.WorkerReviewDiffPath || packet.ReviewContext.DiffBytes != int64(len(diff)) || packet.ReviewContext.DiffSHA256 != hex.EncodeToString(digest[:]) || packet.ReviewContext.CurrentDiff != "" || packet.ReviewContext.OmittedDiffBytes != 0 || packet.ReviewContext.ChangedPathsCommand != "" || packet.ReviewContext.DiffPathCommand != "" {
		t.Fatalf("packet review context = %#v, want only streamed artifact metadata", packet.ReviewContext)
	}
	artifact, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "review.diff"))
	if err != nil {
		t.Fatalf("read streamed review artifact: %v", err)
	}
	if string(artifact) != diff {
		t.Fatalf("streamed review artifact has %d bytes, want %d", len(artifact), len(diff))
	}
}

// TestSpecificationReviewMaterialisesAnEmptyDiff verifies unchanged
// checkpoints still publish a regular zero-byte artifact with its identity.
func TestSpecificationReviewMaterialisesAnEmptyDiff(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.worktree.diff = ""
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	artifactPath := filepath.Join(launch.Invocation.InvocationDirectory, "review.diff")
	info, err := os.Lstat(artifactPath)
	if err != nil {
		t.Fatalf("stat empty review artifact: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("empty review artifact info = %#v, want regular zero-byte file", info)
	}
	packetData, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read empty review packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(packetData, &packet); err != nil {
		t.Fatalf("decode empty review packet: %v", err)
	}
	emptyDigest := sha256.Sum256(nil)
	if packet.ReviewContext == nil || packet.ReviewContext.DiffPath != prompt.WorkerReviewDiffPath || packet.ReviewContext.DiffBytes != 0 || packet.ReviewContext.DiffSHA256 != hex.EncodeToString(emptyDigest[:]) {
		t.Fatalf("empty review context = %#v, want zero-byte artifact identity", packet.ReviewContext)
	}
}

// TestSpecificationReviewCaptureFailureLeavesNoLaunch verifies a source error
// removes its temporary file and prevents every activation side effect.
func TestSpecificationReviewCaptureFailureLeavesNoLaunch(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.worktree.streamErr = errors.New("fixture stream failed")
	_, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err == nil || !strings.Contains(err.Error(), "capture exact review diff") {
		t.Fatalf("StartAgent() error = %v, want streamed capture failure", err)
	}
	if len(fixture.harness.starts) != 0 || len(fixture.worker.starts) != 0 || len(fixture.statuses.values) != 0 || len(fixture.runStore.invocations) != 0 {
		t.Fatalf("capture failure activated effects: harness=%#v worker=%#v statuses=%#v invocations=%#v", fixture.harness.starts, fixture.worker.starts, fixture.statuses.values, fixture.runStore.invocations)
	}
	artifactPath := filepath.Join(fixture.root, ".factory-agents", "run-review", "review-session", "packet", "review.diff")
	if _, statErr := os.Lstat(artifactPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("review artifact after capture failure: stat error = %v, want no published artifact", statErr)
	}
}

func TestSpecificationReviewRefusesReadinessWithoutFinalCheckpointGates(t *testing.T) {
	fixture := newReviewFixture(t)
	run := fixture.runStore.current
	results := fixture.runStore.gateResults[run.ID]
	for index := range results {
		if results[index].Phase == store.GatePhaseCheckpoint {
			results[index].Outcome = store.GateOutcomeFailed
			results[index].Status = string(github.CommitStatusFailure)
		}
	}
	fixture.runStore.gateResults[run.ID] = results

	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	value := reviewReport(launch, nil)
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	_, err = fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err == nil || !strings.Contains(err.Error(), "final checkpoint gate") {
		t.Fatalf("AcceptAgentReport() error = %v, want final gate refusal", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || fixture.runStore.current.Status != store.StatusActive {
		t.Fatalf("run after readiness refusal = %#v, want review/active retry state", fixture.runStore.current)
	}
	if !fixture.pullRequests.existing.Draft {
		t.Fatal("pull request became ready despite an unsuccessful final gate")
	}
}

// TestSpecificationReviewRefusesAnUnreviewedPullRequestHead verifies readiness
// cannot make a PR non-draft when its remote branch moved after review.
func TestSpecificationReviewRefusesAnUnreviewedPullRequestHead(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.pullRequests.existing.HeadSHA = strings.Repeat("a", 64)
	fixture.pullRequests.existing.Draft = false
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	value := reviewReport(launch, nil)
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	_, err = fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID})
	if err == nil || !strings.Contains(err.Error(), "head") {
		t.Fatalf("AcceptAgentReport() error = %v, want remote-head refusal", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || !fixture.pullRequests.existing.Draft {
		t.Fatalf("run/PR after remote-head refusal = %#v/%#v, want review and draft", fixture.runStore.current, fixture.pullRequests.existing)
	}
}

// TestSpecificationReviewRestoresDraftAfterAConcurrentHeadChange verifies a
// race during the readiness mutation cannot leave the unreviewed PR ready.
func TestSpecificationReviewRestoresDraftAfterAConcurrentHeadChange(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.pullRequests.readyHeadSHA = strings.Repeat("a", 64)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	value := reviewReport(launch, nil)
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	_, err = fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID})
	if err == nil || !strings.Contains(err.Error(), "head") {
		t.Fatalf("AcceptAgentReport() error = %v, want readiness race refusal", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || !fixture.pullRequests.existing.Draft {
		t.Fatalf("run/PR after readiness race = %#v/%#v, want review and draft", fixture.runStore.current, fixture.pullRequests.existing)
	}
}

// TestSpecificationReviewBlocksOnlyConcreteViolations verifies a correctness
// blocker becomes one bounded implementation repair packet and publishes
// failure for the exact SHA.
func TestSpecificationReviewBlocksOnlyConcreteViolations(t *testing.T) {
	fixture := newReviewFixture(t)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	reportValue := reviewReport(launch, []report.ReviewFinding{{
		Location: "internal/factory/review.go:1", Claim: "checkpoint is not immutable", Evidence: "worktree can be changed", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryCorrectness, SuggestedResolution: "reject changed paths", SuggestedOwner: "implementation",
	}})
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if fixture.runStore.current.Stage != store.StageImplementation || fixture.runStore.current.Status != store.StatusActive {
		t.Fatalf("blocked run = %#v, want implementation/active repair", fixture.runStore.current)
	}
	if fixture.runStore.current.ReviewRepairAttempts != 1 || fixture.runStore.current.ReviewRepairPacket == nil || len(fixture.runStore.current.ReviewRepairPacket.Findings) != 1 {
		t.Fatalf("review repair projection = %#v, want one bounded finding packet", fixture.runStore.current)
	}
	if len(fixture.runStore.current.ActiveInvocationIDs) != 1 {
		t.Fatalf("active repair invocations = %#v, want one implementation session", fixture.runStore.current.ActiveInvocationIDs)
	}
	if got := fixture.statuses.values[len(fixture.statuses.values)-1].State; got != github.CommitStatusFailure {
		t.Fatalf("final review status = %q, want failure", got)
	}
}

// TestConcurrentReviewRolesUseIndependentCheckpointSessions verifies that the
// specification and standards reviewers can be live at once without sharing a
// worker identity, prompt context, or durable result projection.
func TestConcurrentReviewRolesUseIndependentCheckpointSessions(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults["standards_review"] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions["standards_review"] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save concurrent review packet: %v", err)
	}
	specification, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: "spec_review", Stage: store.StageReview,
	})
	if err != nil {
		t.Fatalf("start specification reviewer: %v", err)
	}
	standards, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: "standards_review", Stage: store.Stage("standards_review"),
	})
	if err != nil {
		t.Fatalf("start standards reviewer: %v", err)
	}
	if specification.Invocation.ID == standards.Invocation.ID || len(fixture.runStore.current.ActiveInvocationIDs) != 2 || fixture.runStore.current.Stage != store.StageReview {
		t.Fatalf("concurrent run projection = %#v, want two active review invocations at review stage", fixture.runStore.current)
	}
	if len(fixture.worker.starts) != 2 || fixture.worker.starts[0].WorkerID == fixture.worker.starts[1].WorkerID || fixture.worker.starts[0].WorkerID == "" || fixture.worker.starts[1].WorkerID == "" {
		t.Fatalf("review worker identities = %#v, want two isolated workers", fixture.worker.starts)
	}
	for _, start := range fixture.worker.starts {
		if !start.WorktreeReadOnly {
			t.Fatalf("review worker start = %#v, want read-only checkpoint mount", start)
		}
	}
	if len(fixture.statuses.values) != 2 || fixture.statuses.values[0].Context != factory.SpecificationReviewStatusContext || fixture.statuses.values[1].Context != factory.StandardsReviewStatusContext {
		t.Fatalf("review status contexts = %#v, want one stable context per role", fixture.statuses.values)
	}
	if !strings.Contains(strings.ToLower(standards.Prompt), "non-overridable factory safety rules") || strings.Contains(standards.Prompt, "specification reviewer's conclusions") == false {
		t.Fatalf("standards prompt does not contain isolated precedence rules:\n%s", standards.Prompt)
	}
	standardsPacketData, err := os.ReadFile(filepath.Join(standards.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read standards packet: %v", err)
	}
	var standardsPacket factory.InvocationPacket
	if err := json.Unmarshal(standardsPacketData, &standardsPacket); err != nil {
		t.Fatalf("decode standards packet: %v", err)
	}
	if standardsPacket.ReviewContext == nil || standardsPacket.ReviewContext.ImplementationHandoff != nil || standardsPacket.ReviewContext.TestHandoff != nil || len(standardsPacket.ReviewContext.ProtectedTestPaths) != 0 || standardsPacket.ReviewContext.TestExemption == nil {
		t.Fatalf("standards review context = %#v, want only deliberate exemption among upstream test signals", standardsPacket.ReviewContext)
	}
	if standardsPacket.TestHandoff != nil || standardsPacket.TestObjection != nil || standardsPacket.TestExemption != nil || len(standardsPacket.ProtectedTestPaths) != 0 {
		t.Fatalf("standards packet inherited test context: %#v", standardsPacket)
	}

	for _, launch := range []factory.AgentLaunchResult{specification, standards} {
		value := reviewReport(launch, nil)
		if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
			t.Fatalf("write %s review report: %v", launch.Invocation.Role, err)
		}
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: specification.Invocation.ID}); err != nil {
		t.Fatalf("accept specification review: %v", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || len(fixture.runStore.current.ActiveInvocationIDs) != 1 {
		t.Fatalf("after specification acceptance = %#v, want one standards reviewer remaining", fixture.runStore.current)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: standards.Invocation.ID}); err != nil {
		t.Fatalf("accept standards review: %v", err)
	}
	if fixture.runStore.current.Stage != store.StageReady || fixture.runStore.current.Status != store.StatusActive || fixture.runStore.current.StandardsReview == nil {
		t.Fatalf("completed review round = %#v, want ready with standards result", fixture.runStore.current)
	}
}

// TestIncompleteReviewFindingRoutesAfterTheOtherAxisTerminates verifies a
// blocking finding survives a non-completed review outcome and enters the same
// bounded repair path after the concurrent axis reports.
func TestIncompleteReviewFindingRoutesAfterTheOtherAxisTerminates(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleStandardsReview] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions[workflow.RoleStandardsReview] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save concurrent review packet: %v", err)
	}
	specification, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview})
	if err != nil {
		t.Fatalf("start specification reviewer: %v", err)
	}
	standards, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: workflow.RoleStandardsReview, Stage: workflow.StageStandardsReview})
	if err != nil {
		t.Fatalf("start standards reviewer: %v", err)
	}
	value := reviewReport(specification, []report.ReviewFinding{{
		Location: "internal/factory/review.go:42", Claim: "the behavior is incorrect", Evidence: "the observed behavior fails",
		Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryCorrectness,
		SuggestedResolution: "repair the behavior", SuggestedOwner: workflow.RoleImplementation,
	}})
	value.Outcome = report.OutcomeCannotProceed
	value.ReviewHandoff = &report.ReviewHandoff{ReviewedSHA: reviewCheckpoint, Findings: value.ReviewHandoff.Findings}
	value.Evidence = []report.Evidence{{Kind: "artifact", Detail: "the required artifact is unavailable"}}
	if _, err := report.WriteAtomicForInvocation(specification.Invocation.ResultDirectory, specification.Invocation.ID, value); err != nil {
		t.Fatalf("write incomplete specification review: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: specification.Invocation.ID}); err != nil {
		t.Fatalf("accept incomplete specification review: %v", err)
	}
	if fixture.runStore.current.SpecificationReview == nil || fixture.runStore.current.SpecificationReview.Outcome != string(report.OutcomeCannotProceed) || len(fixture.runStore.current.SpecificationReview.Findings) != 1 || fixture.runStore.current.Stage != store.StageReview {
		t.Fatalf("incomplete specification review = %#v, want durable finding while other axis is active", fixture.runStore.current)
	}
	standardsValue := reviewReport(standards, nil)
	standardsValue.Outcome = report.OutcomeNeedsClarification
	standardsValue.ReviewHandoff = nil
	standardsValue.Questions = []report.Question{{ID: "missing-artifact", Prompt: "Which artifact should be reviewed by the standards axis?"}}
	if _, err := report.WriteAtomicForInvocation(standards.Invocation.ResultDirectory, standards.Invocation.ID, standardsValue); err != nil {
		t.Fatalf("write standards review: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: standards.Invocation.ID}); err != nil {
		t.Fatalf("accept standards review: %v", err)
	}
	if fixture.runStore.current.Stage != store.StageImplementation || fixture.runStore.current.Status != store.StatusActive || fixture.runStore.current.ReviewRepairPacket == nil || len(fixture.runStore.current.ReviewRepairPacket.Findings) != 1 {
		t.Fatalf("repair projection = %#v, want one finding from incomplete review", fixture.runStore.current)
	}
}

// TestIncompleteReviewWithoutFindingsWaitsForHumanDisposition verifies an
// incomplete no-finding axis does not become a passing review or trigger repair.
func TestIncompleteReviewWithoutFindingsWaitsForHumanDisposition(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleStandardsReview] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions[workflow.RoleStandardsReview] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save concurrent review packet: %v", err)
	}
	specification, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview})
	if err != nil {
		t.Fatalf("start specification reviewer: %v", err)
	}
	standards, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: workflow.RoleStandardsReview, Stage: workflow.StageStandardsReview})
	if err != nil {
		t.Fatalf("start standards reviewer: %v", err)
	}
	value := reviewReport(specification, nil)
	value.Outcome = report.OutcomeNeedsClarification
	value.ReviewHandoff = nil
	value.Questions = []report.Question{{ID: "missing-artifact", Prompt: "Which artifact should be reviewed?"}}
	if _, err := report.WriteAtomicForInvocation(specification.Invocation.ResultDirectory, specification.Invocation.ID, value); err != nil {
		t.Fatalf("write incomplete specification review: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: specification.Invocation.ID}); err != nil {
		t.Fatalf("accept incomplete specification review: %v", err)
	}
	standardsValue := reviewReport(standards, nil)
	standardsValue.Outcome = report.OutcomeNeedsClarification
	standardsValue.ReviewHandoff = nil
	standardsValue.Questions = []report.Question{{ID: "missing-artifact", Prompt: "Which artifact should be reviewed by the standards axis?"}}
	if _, err := report.WriteAtomicForInvocation(standards.Invocation.ResultDirectory, standards.Invocation.ID, standardsValue); err != nil {
		t.Fatalf("write standards review: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: standards.Invocation.ID}); err != nil {
		t.Fatalf("accept standards review: %v", err)
	}
	current := fixture.runStore.current
	if current.Stage != store.StageReview || current.Status != store.StatusWaitingForHuman || current.ReviewRepairPacket != nil || current.SpecificationReview == nil || current.SpecificationReview.Outcome != string(report.OutcomeNeedsClarification) {
		t.Fatalf("incomplete no-finding review = %#v, want explicit human wait without repair", current)
	}
	status := factory.StatusCommentBody(*current)
	for _, marker := range []string{"needs_clarification", "Which artifact should be reviewed?", "missing-artifact"} {
		if !strings.Contains(status, marker) {
			t.Errorf("status comment missing %q: %s", marker, status)
		}
	}
}

// TestConcurrentReviewBlockersBecomeOneRepairPacket verifies the complete
// review round routes both isolated reviewers' blockers to one implementation
// repair invocation instead of repairing from whichever report arrived last.
func TestConcurrentReviewBlockersBecomeOneRepairPacket(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults["standards_review"] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions["standards_review"] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save concurrent review packet: %v", err)
	}
	specification, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: "spec_review", Stage: store.StageReview})
	if err != nil {
		t.Fatalf("start specification reviewer: %v", err)
	}
	standards, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: run.ID, Role: "standards_review", Stage: store.Stage("standards_review")})
	if err != nil {
		t.Fatalf("start standards reviewer: %v", err)
	}
	specificationReport := reviewReport(specification, []report.ReviewFinding{{
		Location: "internal/factory/review.go:10", Claim: "specification blocker", Evidence: "the acceptance path is incomplete", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryCorrectness, SuggestedResolution: "complete the path", SuggestedOwner: "implementation",
	}})
	standardsReport := reviewReport(standards, []report.ReviewFinding{{
		Location: "internal/factory/review.go:20", Claim: "standards blocker", Evidence: "source=guidance:AGENTS.md;hunk=internal/factory/review.go:20;the lifecycle rule is violated", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryDocumentedStandards, SuggestedResolution: "follow the documented rule", SuggestedOwner: "test",
	}})
	if _, err := report.WriteAtomicForInvocation(specification.Invocation.ResultDirectory, specification.Invocation.ID, specificationReport); err != nil {
		t.Fatalf("write specification report: %v", err)
	}
	if _, err := report.WriteAtomicForInvocation(standards.Invocation.ResultDirectory, standards.Invocation.ID, standardsReport); err != nil {
		t.Fatalf("write standards report: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: specification.Invocation.ID}); err != nil {
		t.Fatalf("accept specification review: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: standards.Invocation.ID}); err != nil {
		t.Fatalf("accept standards review: %v", err)
	}
	current := fixture.runStore.current
	if current.Stage != store.StageImplementation || current.Status != store.StatusActive || current.ReviewRepairPacket == nil {
		t.Fatalf("review repair run = %#v, want active implementation repair", current)
	}
	if len(current.ReviewRepairPacket.Findings) != 2 || current.ReviewRepairPacket.Findings[0].ReviewerRole != "spec_review" || current.ReviewRepairPacket.Findings[1].ReviewerRole != "standards_review" {
		t.Fatalf("combined review packet = %#v, want both reviewers in declaration order", current.ReviewRepairPacket)
	}
	if len(current.ActiveInvocationIDs) != 1 {
		t.Fatalf("repair active invocations = %#v, want one implementation invocation", current.ActiveInvocationIDs)
	}
	// This run has no earlier implementation invocation to resume, so the repair
	// starts a fresh session and must receive the complete prompt rather than a
	// continuation of a session that was never opened.
	if len(fixture.harness.resumes) != 0 {
		t.Fatalf("repair resumes = %#v, want a fresh session when no native session exists", fixture.harness.resumes)
	}
	repairPrompt := fixture.harness.starts[len(fixture.harness.starts)-1].Prompt
	for _, present := range []string{"--- BEGIN SPECIFICATION PACKET ---", "--- BEGIN REPOSITORY GUIDANCE ---", "Review-repair packet (coordinator-owned):", "specification blocker"} {
		if !strings.Contains(repairPrompt, present) {
			t.Fatalf("fresh repair prompt missing %q:\n%s", present, repairPrompt)
		}
	}
	if strings.Contains(repairPrompt, "(continuation)") {
		t.Fatalf("fresh repair prompt is a continuation:\n%s", repairPrompt)
	}
}

// TestReviewRejectsAStaleCheckpointWithATypedFailure verifies a result cannot
// silently attach to a newer immutable checkpoint.
func TestReviewRejectsAStaleCheckpointWithATypedFailure(t *testing.T) {
	fixture := newReviewFixture(t)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	value := reviewReport(launch, nil)
	value.ReviewHandoff.ReviewedSHA = factoryGateCheckpoint
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write stale review report: %v", err)
	}
	_, err = fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID})
	var mismatch *factory.ReviewCheckpointMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("AcceptAgentReport() error = %v, want ReviewCheckpointMismatchError", err)
	}
	if mismatch.Expected != reviewCheckpoint || mismatch.Observed != factoryGateCheckpoint || mismatch.Role != launch.Invocation.Role || mismatch.InvocationID != launch.Invocation.ID {
		t.Fatalf("checkpoint mismatch = %#v, want exact role and SHA identities", mismatch)
	}
	if fixture.runStore.current.Stage != store.StageReview || len(fixture.runStore.current.ActiveInvocationIDs) != 1 {
		t.Fatalf("run after stale review = %#v, want active review unchanged", fixture.runStore.current)
	}
}

// TestStandardsReviewCanBlockAProvisionalTestExemption verifies the standards
// role receives the deliberate exemption signal and can reject it as a
// documented-standards violation without inheriting implementation context.
func TestStandardsReviewCanBlockAProvisionalTestExemption(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults["standards_review"] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions["standards_review"] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save standards review packet: %v", err)
	}
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: "standards_review", Stage: store.Stage("standards_review"),
	})
	if err != nil {
		t.Fatalf("start standards reviewer: %v", err)
	}
	value := reviewReport(launch, []report.ReviewFinding{{
		Location: "issue body", Claim: "the provisional test exemption violates repository standards", Evidence: "source=guidance:CONTEXT.md;hunk=issue body;the exemption is not allowed by the frozen policy", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryDocumentedStandards, SuggestedResolution: "satisfy the required test policy", SuggestedOwner: "implementation",
	}})
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write standards review report: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("accept standards review: %v", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || fixture.runStore.current.Status != store.StatusActive || fixture.runStore.current.StandardsReview == nil {
		t.Fatalf("standards-blocked run = %#v, want active review waiting for the other reviewer", fixture.runStore.current)
	}
	if got := fixture.statuses.values[len(fixture.statuses.values)-1]; got.Context != factory.StandardsReviewStatusContext || got.State != github.CommitStatusFailure {
		t.Fatalf("standards status = %#v, want failure in standards context", got)
	}
}

// TestStandardsReviewLaunchCanRetryFromDraftPullRequest verifies a voided
// standards-review launch reopens its declared invocation boundary rather than
// being rejected by the draft-pull-request retry path.
func TestStandardsReviewLaunchCanRetryFromDraftPullRequest(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	var packet factory.SpecificationPacket
	if err := json.Unmarshal([]byte(run.SpecificationPacket), &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleStandardsReview] = config.HarnessCodex
	packet.RepositoryConfig.ModelOptions[workflow.RoleStandardsReview] = []string{"gpt-5"}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("encode review packet: %v", err)
	}
	run.SpecificationPacket = string(packetData)
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save standards review packet: %v", err)
	}

	fixture.harness.startErr = errors.New("standards reviewer unavailable")
	_, err = fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: workflow.RoleStandardsReview, Stage: workflow.StageStandardsReview,
	})
	if err == nil || !strings.Contains(err.Error(), "use `/factory retry`") {
		t.Fatalf("StartAgent(standards review) error = %v, want void-launch retry guidance", err)
	}
	failed, err := fixture.runStore.LatestInvocation(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read failed standards invocation: %v", err)
	}
	if failed == nil || failed.Status != store.InvocationStatusSuperseded || !failed.LaunchVoided {
		t.Fatalf("failed standards invocation = %#v, want a voided superseded launch", failed)
	}

	run = *fixture.runStore.current
	run.Status = store.StatusWaitingForHuman
	run.ActiveInvocationIDs = nil
	run.LifecycleReason = "unattended progression stopped at standards review launch: harness launch produced no native session or structured report; use /factory retry"
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist waiting standards launch: %v", err)
	}

	retry, err := fixture.service.HandleCommand(context.Background(), factory.CommandRequest{
		RunID: run.ID, IssueNumber: run.IssueNumber,
		Comment: github.Comment{ID: "retry-standards-launch", Author: "alice", Body: "/factory retry"},
	})
	if err != nil {
		t.Fatalf("HandleCommand(standards review) error = %v", err)
	}
	if retry.Outcome != factory.CommandAccepted || retry.Run.Status != store.StatusActive || retry.Run.Stage != store.StageDraftPR {
		t.Fatalf("standards retry result = %#v, want accepted active draft_pr run", retry)
	}

	fixture.harness.startErr = nil
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: workflow.RoleStandardsReview, Stage: workflow.StageStandardsReview,
	})
	if err != nil {
		t.Fatalf("StartAgent(standards review) after retry error = %v", err)
	}
	if launch.Invocation.ID == failed.ID || launch.Invocation.Role != workflow.RoleStandardsReview || launch.Invocation.Stage != workflow.StageStandardsReview {
		t.Fatalf("recovered standards invocation = %#v, want a fresh standards_review/standards_review invocation", launch.Invocation)
	}
}

// TestReviewLaunchDoesNotPublishPendingStatusBeforeMaterialisation verifies
// the lifecycle ordering carve-out: a packet-directory failure leaves GitHub
// untouched because status publication belongs to activation.
func TestReviewLaunchDoesNotPublishPendingStatusBeforeMaterialisation(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	blockedParent := filepath.Join(filepath.Dir(run.Worktree), "blocked-materialisation")
	if err := os.WriteFile(blockedParent, []byte("directory blocker"), 0o600); err != nil {
		t.Fatalf("create materialisation blocker: %v", err)
	}
	run.Worktree = filepath.Join(blockedParent, "worktree")
	if err := fixture.runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save blocked review run: %v", err)
	}
	before := len(fixture.statuses.values)
	_, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview,
	})
	if err == nil || !strings.Contains(err.Error(), "create invocation packet directory") {
		t.Fatalf("StartAgent() error = %v, want materialisation failure", err)
	}
	if got := len(fixture.statuses.values); got != before {
		t.Fatalf("commit statuses = %d, want unchanged at %d", got, before)
	}
}

// reviewFixture bundles the Factory seam and all isolated review projections.
type reviewFixture struct {
	root         string
	service      *factory.Service
	runStore     *agentRunStore
	worker       *agentWorker
	worktree     *reviewDiffWorktree
	statuses     *gateStatuses
	pullRequests *fakePullRequests
	harness      *agentHarness
}

// newReviewFixture creates a draft-PR run with a valid baseline and checkpoint.
func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["spec_review"] = config.HarnessCodex
	policy.ModelOptions["spec_review"] = []string{"gpt-5"}
	issue := github.Issue{Number: 42, Title: "Review the checkpoint", Body: "Review the exact implementation checkpoint.", State: "open", Labels: []string{github.LabelAgentReady}}
	githubAdapter := &fakeGitHub{issueValue: issue}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{existing: github.PullRequest{Number: 17, URL: "https://github.com/example/project/pull/17", Body: "<!-- factory-generated:start -->\nold\n<!-- factory-generated:end -->", State: "open", Draft: true, HeadBranch: "factory/run-review", HeadSHA: reviewCheckpoint, BaseBranch: "main"}}
	baseWorktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{
			workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-review", Worktree: worktreePath},
			guidance: []gitadapter.GuidanceDocument{
				{Path: "AGENTS.md", Content: "Follow named repository rules.\n"},
				{Path: "CONTEXT.md", Content: "Keep review axes separate.\n"},
			},
		},
		state: gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-review", HeadSHA: factoryGateCheckpoint},
	}
	worktree := &reviewDiffWorktree{inspectingWorktree: baseWorktree, diff: "diff --git a/internal/factory/review.go b/internal/factory/review.go\n"}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}, worktree: baseWorktree}
	runtime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"},
		Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	ids := []string{"run-review", "review-session", "standards-session", "repair-session"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       worktree,
		Worker:         runtime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	})
	claimed, err := service.ClaimIssue(context.Background(), issue.Number)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	run := claimed.Run
	run.TestStageSkipped = true
	run.TestExemption = &store.TestExemption{Kind: "human", Justification: "review fixture"}
	run.RoleHandoff = &store.RoleHandoff{
		ChangeSummary:          "implemented review behavior",
		AcceptanceMapping:      []store.HandoffAcceptance{{Criterion: "review behavior", Evidence: "focused test"}},
		ProductionFilesChanged: []string{"internal/factory/review.go"},
		FocusedCommands:        []string{"go test ./internal/factory"},
	}
	run.Stage = store.StageDraftPR
	run.Status = store.StatusActive
	run.BaseCheckpointSHA = factoryGateCheckpoint
	run.CheckpointSHA = reviewCheckpoint
	run.PullRequestNumber = pullRequests.existing.Number
	run.PullRequestURL = pullRequests.existing.URL
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() review fixture error = %v", err)
	}
	githubAdapter.statusComment = github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)}
	worktree.state.HeadSHA = reviewCheckpoint
	worktree.state.ChangedPaths = nil
	runStore.gateResults[run.ID] = []store.GateResult{
		{RunID: run.ID, CheckpointSHA: factoryGateCheckpoint, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking},
		{RunID: run.ID, CheckpointSHA: reviewCheckpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking},
	}
	return reviewFixture{root: root, service: service, runStore: runStore, worker: runtime, worktree: worktree, statuses: statuses, pullRequests: pullRequests, harness: harnessRuntime}
}

// reviewDiffWorktree supplies the explicit read-only diff projection used by
// review tests without routing gather through the worker runtime.
type reviewDiffWorktree struct {
	*inspectingWorktree
	diff      string
	streamErr error
}

// StreamDiff writes the exact fixture diff directly to the materialisation
// destination without routing it through the worker runtime.
func (w *reviewDiffWorktree) StreamDiff(_ context.Context, _, _, _ string, destination io.Writer) error {
	if w.streamErr != nil {
		return w.streamErr
	}
	_, err := io.WriteString(destination, w.diff)
	return err
}

// reviewReport creates a valid completed review report for a launched session.
func reviewReport(launch factory.AgentLaunchResult, findings []report.ReviewFinding) report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: launch.Invocation.ID, RunID: launch.Invocation.RunID,
		Harness: launch.Invocation.Harness, Role: launch.Invocation.Role, Stage: string(launch.Invocation.Stage),
		Outcome: report.OutcomeCompleted, Summary: "review complete", ReviewHandoff: &report.ReviewHandoff{
			ReviewedSHA: reviewCheckpoint, Findings: findings,
		}, NativeSessionID: "session-review", ReportedAt: time.Now().UTC(),
	}
}
