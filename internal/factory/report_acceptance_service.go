package factory

import (
	"context"
	"errors"
	"strings"
)

// acceptanceModule builds the report-acceptance module from the coordinator's
// current adapters. It is rebuilt per call, like the journal, so a late
// adapter substitution reaches the module exactly as a direct `s.deps` read
// did before the extraction.
func (s *Service) acceptanceModule() *reportAcceptance {
	return newReportAcceptance(
		s.journal(),
		journalLifecycle{service: s},
		s.worktreeInspector(),
		s.deps.Now,
		reportAcceptanceHooks{
			acceptTestStage:           s.acceptTestStageReport,
			acceptReview:              s.acceptSpecificationReviewReport,
			finishObjection:           s.finishImplementationTestObjection,
			pauseUnverifiableTest:     s.pauseUnverifiableTestReport,
			pauseUnverifiableRevision: s.pauseUnverifiableTestRevisionReport,
			publishClarification:      s.ensureClarificationPublication,
			objectionGate:             s.automatedTestObjectionGate,
			persistRun:                s.persistAgentRunState,
		},
	)
}

// AcceptAgentReport reads only the invocation report file, validates its
// identity and observed worktree state, and then lets the coordinator decide
// the resulting workflow status. Store opening and command locking remain
// coordinator concerns; every phase after them belongs to the module.
func (s *Service) AcceptAgentReport(ctx context.Context, request AgentReportRequest) (AgentResult, error) {
	if strings.TrimSpace(request.InvocationID) == "" {
		return AgentResult{}, errors.New("invocation id is required")
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, run, err := s.openReportRunStore(ctx)
	if err != nil {
		return AgentResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	return s.acceptanceModule().Accept(ctx, ReportAcceptanceRequest{
		Registration:       registration,
		RunStore:           runStore,
		Run:                run,
		Request:            request,
		EvaluationRecorder: acceptanceEvaluationRecorderForRunStore(runStore),
	})
}
