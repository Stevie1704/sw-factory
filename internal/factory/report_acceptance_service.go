package factory

// acceptanceModule builds the report-acceptance module from the coordinator's
// current adapters. It is rebuilt per call, like the journal, so a late
// adapter substitution reaches the module exactly as a direct `s.deps` read
// did before the extraction.
func (s *Service) acceptanceModule(evaluationRecorder acceptanceEvaluationRecorder) *reportAcceptance {
	return newReportAcceptance(
		s.journal(),
		journalLifecycle{service: s},
		s.worktreeInspector(),
		s.deps.Now,
		evaluationRecorder,
		automatedTestObjectionGate,
		reportAcceptanceHooks{
			acceptTestStage:           s.acceptTestStageReport,
			acceptReview:              s.acceptSpecificationReviewReport,
			finishObjection:           s.finishImplementationTestObjection,
			pauseUnverifiableTest:     s.pauseUnverifiableTestReport,
			pauseUnverifiableRevision: s.pauseUnverifiableTestRevisionReport,
			publishClarification:      s.ensureClarificationPublication,
			persistRun:                s.persistAgentRunState,
		},
	)
}
