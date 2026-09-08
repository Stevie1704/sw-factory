package factory

import (
	"context"

	"github.com/Stevie1704/sw-factory/internal/config"
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// journalEntry builds one durable journal entry with the coordinator clock,
// exactly as a production apply path reserves it. Replay tests use it to seed
// an entry for a kind whose apply path takes different inputs, or none.
func journalEntry(service *Service, runID string, kind store.PendingEffectKind, identity string, payload any) (store.PendingEffect, error) {
	return effectkernel.NewPendingEffect(service.deps.Now().UTC(), runID, kind, identity, payload)
}

// reserveThen runs one action under the journal's reserve/apply/complete
// protocol so a test can observe the reservation boundary directly.
func reserveThen(ctx context.Context, runStore effectkernel.RunStore, entry store.PendingEffect, action func() error) error {
	return effectkernel.WithPendingEffect(ctx, runStore, entry, journalAction(action))
}

// journalAction adapts a test action to the kernel's application seam.
type journalAction func() error

// Apply runs the test action.
func (a journalAction) Apply() error { return a() }

// acceptResultWithEffect resolves a repository registration to the values the
// journal takes, mirroring the coordinator's own acceptance call site.
func (s *Service) acceptResultWithEffect(ctx context.Context, runStore effectkernel.RunStore, invocationStore effectkernel.InvocationStore, registration config.RepositoryRegistration, harnessRuntime harness.Runtime, session harness.Session, invocation store.Invocation, previous, next store.Run, stopWorker bool, acceptedReport report.Report) (store.Invocation, store.Run, error) {
	return s.journal().AcceptResult(ctx, runStore, invocationStore, effectkernel.ResultAcceptance{
		Repository: commandRepository(registration),
		SocketPath: registration.Cmux.SocketPath,
		WorkerID:   workerIDForInvocation(invocation),
		Harness:    harnessRuntime,
		Session:    session,
		Invocation: invocation,
		Previous:   previous,
		Next:       next,
		StopWorker: stopWorker,
		Report:     acceptedReport,
	})
}

// The payload mirrors below carry the durable JSON shape of a journal entry.
// Seeding a replay test through them proves the handler decodes an entry it
// did not write itself, which is what a restart across builds actually does.

// journalLabelPayload mirrors a standalone issue-label replacement entry.
type journalLabelPayload struct {
	Repository  github.Repository
	IssueNumber int
	Labels      []string
}

// journalPullRequestPayload mirrors a draft pull-request mutation entry.
type journalPullRequestPayload struct {
	Repository github.Repository
	Number     int
	Request    github.PullRequestRequest
	PersistRun bool
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// journalCheckpointRequest mirrors the serialized Git checkpoint request.
type journalCheckpointRequest struct {
	RunID        string
	WorktreePath string
	ParentSHA    string
	Kind         string
	Paths        []string
	Message      string
}

// journalCheckpointPayload mirrors a checkpoint entry and its run projection.
type journalCheckpointPayload struct {
	Request    journalCheckpointRequest
	Repository github.Repository
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// journalHarnessResumePayload mirrors a native-session continuation entry.
type journalHarnessResumePayload struct {
	SocketPath        string
	Request           harness.StartRequest
	Invocation        store.Invocation
	TargetResumeCount int
	Manual            bool
}

// journalResultAcceptancePayload mirrors an accepted-report entry.
type journalResultAcceptancePayload struct {
	Repository     github.Repository
	SocketPath     string
	Issue          github.Issue
	Session        harness.Session
	Invocation     store.Invocation
	WorkerID       string
	Previous       store.Run
	Next           store.Run
	StopWorker     bool
	AcceptedReport string
}
