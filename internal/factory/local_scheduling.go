package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// localScheduleStore is the durable source, authority, claim and queue seam.
type localScheduleStore interface {
	RunStore
	localExecutionStore
	SupervisorHeartbeatReader
	Run(context.Context, string) (*store.Run, error)
	LocalClaims(context.Context, string) ([]store.LocalClaim, error)
	InvalidateLocalExecution(context.Context, string, string, string, time.Time) error
	LocalExecutionInvalidations(context.Context, string) (map[string]string, error)
	ReserveLocalClaim(context.Context, store.LocalClaim) (store.LocalClaim, error)
	ReleaseLocalClaim(context.Context, store.LocalClaim) error
	SaveLocalClaimRun(context.Context, store.LocalClaim, store.Run) (store.Run, error)
	SaveLocalBacklog(context.Context, string, store.LocalBacklogProjection) error
	LocalBacklog(context.Context, string) (store.LocalBacklogProjection, error)
	IdeationSessions(context.Context, string) ([]store.IdeationSession, error)
	IdeationHistory(context.Context, string) (store.IdeationHistory, error)
}

// LocalBacklogView is read-only last-observed source and structured Run evidence.
type LocalBacklogView struct {
	Supervisor SupervisorView
	Projection store.LocalBacklogProjection
}

// LocalBacklog never contacts Git or a remote service, including while stopped.
func (s *Service) LocalBacklog(ctx context.Context) (LocalBacklogView, error) {
	r, err := s.registration()
	if err != nil {
		return LocalBacklogView{}, err
	}
	if r.EffectiveTracker() != config.ProviderLocal {
		return LocalBacklogView{}, errors.New("local backlog is unavailable for this tracker")
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, r.OperationalDataPath)
	if err != nil {
		return LocalBacklogView{}, err
	}
	defer opened.Close()
	db, ok := opened.(localScheduleStore)
	if !ok {
		return LocalBacklogView{}, errors.New("store lacks local backlog projections")
	}
	supervisor, err := s.supervisorLiveness(ctx, db)
	if err != nil {
		return LocalBacklogView{}, err
	}
	projection, err := db.LocalBacklog(ctx, r.Path)
	if err != nil {
		return LocalBacklogView{}, err
	}
	claims, err := db.LocalClaims(ctx, r.Path)
	if err != nil {
		return LocalBacklogView{}, err
	}
	auths, err := db.LocalExecutionAuthorizations(ctx, r.Path)
	if err != nil {
		return LocalBacklogView{}, err
	}
	latest := map[string]store.LocalExecutionAuthorization{}
	for _, auth := range auths {
		for _, id := range auth.Binding.SelectedIssueIDs {
			latest[id] = auth
		}
	}
	for i := range projection.Items {
		item := &projection.Items[i]
		auth, approved := latest[item.Issue.ID]
		if !approved || item.State == "stale_authorization" || item.State == "source_unavailable" {
			continue
		}
		for _, claim := range claims {
			if claim.RevisionKey() == localClaimKey(auth.Binding, item.Issue) {
				if err := projectLocalClaim(ctx, db, item, claim); err != nil {
					return LocalBacklogView{}, err
				}
			}
		}
	}
	if active, err := db.CurrentRun(ctx); err != nil {
		return LocalBacklogView{}, err
	} else {
		linkActiveLocalRun(&projection, active)
	}
	return LocalBacklogView{Supervisor: supervisor, Projection: projection}, nil
}

// localAdmission retains the one immutable observation used to freeze a claim.
type localAdmission struct {
	item     store.LocalBacklogItem
	auth     store.LocalExecutionAuthorization
	snapshot tracker.CommittedSnapshot
}

// publicationEvidence is proven local ledger provenance, never file metadata.
type publicationEvidence struct {
	sessionID, branch, commit, policy string
	complete                          bool
	preview                           tracker.PlanPreview
}

// localPublicationEvidence joins accepted previews to actual session/ledger data.
func localPublicationEvidence(ctx context.Context, db localScheduleStore, repo string) (map[string]publicationEvidence, []store.LocalBacklogItem, error) {
	result := map[string]publicationEvidence{}
	pending := []store.LocalBacklogItem{}
	sessions, err := db.IdeationSessions(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	for _, session := range sessions {
		history, err := db.IdeationHistory(ctx, session.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, approval := range history.Approvals {
			preview, err := publication.DecodePreview(approval.Payload, approval.RelationshipMapping)
			if err != nil || !strings.HasPrefix(preview.Target, "local:") {
				continue
			}
			evidence := publicationEvidence{sessionID: session.ID, preview: preview}
			hasBatch := false
			all := true
			for _, op := range history.Operations {
				if op.ApprovalID != approval.ID {
					continue
				}
				all = all && op.State == store.PublicationOperationSucceeded
				if op.Kind == store.PublicationOperationGitBatch {
					hasBatch = true
					var detail publication.BatchOperationDetail
					if json.Unmarshal([]byte(op.Detail), &detail) == nil {
						evidence.branch, evidence.commit = detail.Evidence.Branch, detail.Evidence.CommitSHA
						all = all && detail.Evidence.Complete
					} else {
						all = false
					}
				}
			}
			published := false
			for _, receipt := range history.Receipts {
				if receipt.ApprovalID == approval.ID {
					published = receipt.Outcome == store.PublicationOutcomePublished
				}
			}
			evidence.complete = hasBatch && all && published && session.State == store.IdeationStatePublished
			for _, rendered := range preview.Items {
				source, err := localissues.Parse(rendered.Fields["path"], []byte(rendered.Body))
				if err != nil {
					return nil, nil, err
				}
				number, err := strconv.Atoi(rendered.Fields["number"])
				if err != nil {
					return nil, nil, err
				}
				evidence.policy = rendered.Fields["publication_policy"]
				key := approval.ID + "\x00" + session.ID
				result[key] = evidence
				issue := store.LocalExecutionIssue{ID: source.ID, Number: number, Path: source.Path, Bytes: source.Bytes, ContentDigest: source.ContentDigest, Kind: source.Kind, Parent: source.Parent, Dependencies: source.Dependencies}
				pending = append(pending, store.LocalBacklogItem{Issue: issue, Title: source.Title, State: "missing_execution_approval", Reason: "accepted publication is content only; explicit execution approval is required", SessionID: session.ID, PublicationBranch: evidence.branch, PublicationCommit: evidence.commit, PublicationPolicy: evidence.policy})
			}
		}
	}
	return result, pending, nil
}

// localHostReady verifies actual independent capabilities before any claim effect.
func (s *Service) localHostReady(ctx context.Context, r config.RepositoryRegistration) error {
	if s.pullRequestClient() == nil || s.deps.CommitStatuses == nil || s.deps.PullRequestReviews == nil || s.pullRequestCommentReader() == nil || s.pullRequestCommentClient() == nil || s.deps.CodeHostReadiness == nil {
		return errors.New("configured code host lacks PR, review, status, comment, or readiness capability; recompose supported adapters")
	}
	if _, ok := s.deps.CommitStatuses.(codehost.CommitStatusReader); !ok {
		return errors.New("code-host status inspection is unavailable")
	}
	report := doctor.Run(ctx, s.deps.CodeHostReadiness.CodeHostStartupChecks(registeredRepository(r))...)
	if !report.Ready() {
		var reasons []string
		for _, failure := range report.Failures() {
			reasons = append(reasons, failure.Problem+"; "+failure.Action)
		}
		return errors.New(strings.Join(reasons, "; "))
	}
	return nil
}

// observeLocalQueue validates authority and retains reasons for every item.
func (s *Service) observeLocalQueue(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore) ([]localAdmission, error) {
	r, err := s.currentLocalRegistration(r)
	if err != nil {
		return nil, err
	}
	prior, err := db.LocalBacklog(ctx, r.Path)
	if err != nil {
		return nil, err
	}
	invalidations, err := db.LocalExecutionInvalidations(ctx, r.Path)
	if err != nil {
		return nil, err
	}
	snapshot, sourceErr := s.refreshLocalSource(ctx, r)
	if sourceErr != nil {
		if errors.Is(sourceErr, tracker.ErrInvalidCommittedSource) {
			auths, err := db.LocalExecutionAuthorizations(ctx, r.Path)
			if err != nil {
				return nil, err
			}
			for _, auth := range auths {
				if err := db.InvalidateLocalExecution(ctx, r.Path, auth.RequestID, sourceErr.Error(), s.deps.Now()); err != nil {
					return nil, err
				}
			}
		}
		prior.Error = sourceErr.Error()
		prior.ObservedAt = s.deps.Now()
		for i := range prior.Items {
			prior.Items[i].State = "source_unavailable"
			prior.Items[i].Reason = "committed source could not be validated: " + sourceErr.Error()
		}
		return nil, db.SaveLocalBacklog(ctx, r.Path, prior)
	}
	projection := store.LocalBacklogProjection{Revision: snapshot.Revision, GraphDigest: snapshot.Graph.Digest, ObservedAt: s.deps.Now(), Items: []store.LocalBacklogItem{}}
	auths, err := db.LocalExecutionAuthorizations(ctx, r.Path)
	if err != nil {
		return nil, err
	}
	latest := map[string]store.LocalExecutionAuthorization{}
	for _, auth := range auths {
		for _, id := range auth.Binding.SelectedIssueIDs {
			latest[id] = auth
		}
	}
	// Invalidation is graph-wide and independent of the source's current kind.
	// A task edited to a parent/investigation cannot revive an old grant on revert.
	seenAuth := map[string]bool{}
	for _, auth := range latest {
		if seenAuth[auth.RequestID] || invalidations[auth.RequestID] != "" {
			continue
		}
		seenAuth[auth.RequestID] = true
		reason := ""
		if err := s.executionBindingCurrent(r, auth.Binding); err != nil {
			reason = err.Error()
		} else if state, detail := localVisibility(auth.Binding, snapshot); state == "stale_authorization" {
			reason = detail
		}
		if reason != "" {
			if err := db.InvalidateLocalExecution(ctx, r.Path, auth.RequestID, reason, s.deps.Now()); err != nil {
				return nil, err
			}
			invalidations[auth.RequestID] = reason
		}
	}
	evidence, pending, err := localPublicationEvidence(ctx, db, r.Path)
	if err != nil {
		return nil, err
	}
	items := map[string]store.LocalBacklogItem{}
	actualIDs := map[string]bool{}
	for _, source := range snapshot.Issues {
		actualIDs[source.Source.ID] = true
	}
	for _, item := range pending {
		items[item.Issue.ID] = item
	}
	for _, source := range snapshot.Issues {
		old := items[source.Source.ID]
		old.Issue = store.LocalExecutionIssue{ID: source.Source.ID, Number: source.Number, Path: source.Source.Path, ContentDigest: source.Source.ContentDigest, Bytes: source.Source.Bytes, Kind: source.Source.Kind, Parent: source.Source.Parent, Dependencies: source.Source.Dependencies}
		old.Title = source.Source.Title
		items[source.Source.ID] = old
	}
	for _, auth := range auths {
		for _, source := range auth.Binding.Issues {
			if _, exists := items[source.ID]; !exists {
				parsed, err := localissues.Parse(source.Path, source.Bytes)
				if err != nil {
					return nil, err
				}
				items[source.ID] = store.LocalBacklogItem{Issue: source, Title: parsed.Title}
			}
		}
	}
	claims, err := db.LocalClaims(ctx, r.Path)
	if err != nil {
		return nil, err
	}
	active, err := db.CurrentRun(ctx)
	if err != nil {
		return nil, err
	}
	hostErr := s.localHostReady(ctx, r)
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	admissions := []localAdmission{}
	for _, id := range ids {
		item := items[id]
		item.State = "missing_execution_approval"
		item.Reason = "explicit local execution approval is required"
		auth, approved := latest[id]
		if item.Issue.Kind == localissues.KindParent {
			item.State = "grouping_parent"
			item.Reason = "organizational parents do not create implementation Runs"
		} else if item.Issue.Kind == localissues.KindInvestigation {
			item.State = "unsupported_investigation"
			item.Reason = "no executable investigation route is declared"
		} else if approved {
			item.AuthorizationID = auth.RequestID
			binding := auth.Binding
			authorityIssue := item.Issue
			for _, frozen := range binding.Issues {
				if frozen.ID == id {
					authorityIssue = frozen
					if !actualIDs[id] {
						item.Issue = frozen
					}
					break
				}
			}
			parsed, _ := localissues.Parse(item.Issue.Path, item.Issue.Bytes)
			item.Title = parsed.Title
			pub := evidence[binding.ApprovalID+"\x00"+binding.SessionID]
			if binding.SourceKind == "publication" {
				item.SessionID = binding.SessionID
				item.PublicationBranch, item.PublicationCommit, item.PublicationPolicy = pub.branch, pub.commit, binding.PublicationPolicy
			}
			item.State, item.Reason = "queued", "authorized exact source is ready"
			if reason, invalid := invalidations[auth.RequestID]; invalid {
				item.State, item.Reason = "stale_authorization", "authorization was invalidated: "+reason+"; preview and approve again"
			} else if err := s.executionBindingCurrent(r, binding); err != nil {
				item.State, item.Reason = "stale_authorization", err.Error()
			} else if binding.SourceKind == "publication" && !pub.complete {
				item.State, item.Reason = "publication_incomplete", "the original publication is incomplete or cancelled; reconcile it or explicitly approve the committed target again"
			}
			if item.State == "queued" {
				if state, reason := localVisibility(binding, snapshot); state != "" {
					item.State, item.Reason = state, reason
				}
			}
			if binding.SourceKind == "publication" && pub.complete && (item.State == "queued" || item.State == "waiting_for_specification_merge") {
				batch, ok := s.deps.Tracker.(tracker.PlanBatchPublisher)
				if !ok {
					item.State, item.Reason = "publication_incomplete", "batch publication inspection is unavailable"
				} else if observed, found, err := batch.FindPlanBatch(ctx, registeredRepository(r), pub.preview); err != nil {
					item.State, item.Reason = "publication_incomplete", "specification publication could not be inspected: "+err.Error()+"; the item waits until inspection succeeds"
				} else if !found || !observed.Complete || observed.CommitSHA != pub.commit {
					if item.State == "queued" {
						item.Reason += "; historical specification ref/push/PR changed or was removed, but final publication evidence and exact target content remain established"
					} else {
						item.State, item.Reason = "publication_incomplete", "recorded specification ref/push/PR is unavailable or conflicting before target visibility; reconcile publication"
					}
				}
			}

			if item.State == "queued" {
				for _, dependency := range item.Issue.Dependencies {
					if !s.localDependencyComplete(ctx, db, claims, binding, dependency, snapshot.Revision) {
						item.State = "blocked_by_dependencies"
						item.Reason = "prerequisite " + dependency + " needs successful lifecycle completion of its exact content and its merge commit visible on this exact target; fetch complete target history if unavailable"
						break
					}
				}
			}
			if item.State == "stale_authorization" {
				if err := db.InvalidateLocalExecution(ctx, r.Path, auth.RequestID, item.Reason, s.deps.Now()); err != nil {
					return nil, err
				}
				invalidations[auth.RequestID] = item.Reason
			}
			key := localClaimKey(binding, authorityIssue)
			// A source change after consumption keeps the claim's Run outcome
			// visible. Registration or target drift still shows the stale grant.
			sourceStaleOnly := item.State == "stale_authorization" && s.executionBindingCurrent(r, binding) == nil
			if sourceStaleOnly {
				// The current source keeps its own verdict next to the Run outcome.
				item.QueueState, item.QueueReason = item.State, item.Reason
			}
			for _, claim := range claims {
				if claim.RevisionKey() == key {
					if item.State == "queued" || claim.State == "reserved" || sourceStaleOnly {
						if err := projectLocalClaim(ctx, db, &item, claim); err != nil {
							return nil, err
						}
					} else {
						item.RunID = claim.RunID
					}
				}
			}
			if item.State == "queued" && hostErr != nil {
				item.State, item.Reason = "code_host_unavailable", hostErr.Error()
			}
			if item.State == "queued" && active != nil {
				item.State = "active_run"
				item.Reason = "Run " + active.ID + " retains the repository slot"
			}
			if item.State == "queued" {
				admissions = append(admissions, localAdmission{item: item, auth: auth, snapshot: snapshot})
			}
		}
		if item.QueueState == "" {
			item.QueueState, item.QueueReason = item.State, item.Reason
		}
		projection.Items = append(projection.Items, item)
	}
	linkActiveLocalRun(&projection, active)
	if hostErr != nil {
		projection.Error = hostErr.Error()
	}
	return admissions, db.SaveLocalBacklog(ctx, r.Path, projection)
}

// linkActiveLocalRun distinguishes a Run's frozen revision/activity from the
// current source's pending queue decision, including malformed-source waits.
func linkActiveLocalRun(projection *store.LocalBacklogProjection, run *store.Run) {
	if run == nil || store.IsTerminalStatus(run.Status) {
		return
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil || packet.LocalSource == nil {
		return
	}
	for i := range projection.Items {
		item := &projection.Items[i]
		if item.Issue.ID != packet.LocalSource.IssueID {
			continue
		}
		item.RunID, item.PRURL = run.ID, run.PullRequestURL
		item.RunState = string(run.Status)
		if run.Status == store.StatusActive {
			item.RunState = "executing"
			if run.Stage == store.StageReady {
				item.RunState = "review_ready"
			}
		} else if run.Status == store.StatusWaitingForHuman {
			item.RunState = "awaiting_input"
		}
		item.RunReason = run.LifecycleReason
		item.FrozenRevision, item.FrozenContentDigest = packet.LocalSource.Revision, packet.LocalSource.ContentDigest
		item.State, item.Reason = item.RunState, item.RunReason
		if item.Issue.ContentDigest != packet.LocalSource.ContentDigest || projection.GraphDigest != packet.LocalSource.GraphDigest || projection.Error != "" {
			item.Reason = "Run retains its frozen accepted source; current source/queue: " + item.QueueReason
		}
	}
}

// localVisibility compares exact content/graph while distinguishing a merge wait.
func localVisibility(binding store.LocalExecutionBinding, snapshot tracker.CommittedSnapshot) (string, string) {
	if snapshot.Graph.Digest == binding.GraphDigest {
		return "", ""
	}
	newIDs := map[string]bool{}
	if binding.SourceKind == "publication" {
		preview, err := publication.DecodePreview(binding.Payload, binding.RelationshipMapping)
		if err != nil {
			return "stale_authorization", "approved publication evidence is invalid"
		}
		for _, item := range preview.Items {
			newIDs[item.Fields["local_id"]] = true
		}
	}
	actual := map[string]localissues.Issue{}
	for _, issue := range snapshot.Graph.Issues {
		actual[issue.ID] = issue
	}
	files := make([]localissues.File, 0, len(binding.Issues))
	missing := false
	for _, expected := range binding.Issues {
		if issue, ok := actual[expected.ID]; ok {
			if !bytes.Equal(issue.Bytes, expected.Bytes) {
				return "stale_authorization", "committed issue bytes or relationships changed; preview and approve again"
			}
			files = append(files, localissues.File{Path: issue.Path, Data: issue.Bytes})
			delete(actual, expected.ID)
		} else {
			if !newIDs[expected.ID] {
				return "stale_authorization", "a previously committed baseline issue disappeared; preview and approve again"
			}
			missing = true
			files = append(files, localissues.File{Path: expected.Path, Data: expected.Bytes})
		}
	}
	reconstructed, err := localissues.Validate(files)
	if err != nil || len(actual) != 0 || reconstructed.Digest != binding.GraphDigest {
		return "stale_authorization", "the complete accepted graph changed; preview and approve again"
	}
	if missing && binding.SourceKind == "publication" {
		return "waiting_for_specification_merge", "waiting for specification merge: exact approved files are not on the fetched target"
	}
	return "stale_authorization", "accepted source is missing from the fetched target; preview and approve again"
}

// localClaimKey binds a revision across independent duplicate authorization IDs
// and across graph changes. The binding's graph digest gates authority only.
func localClaimKey(binding store.LocalExecutionBinding, issue store.LocalExecutionIssue) string {
	return store.LocalRevisionKey(binding.InstallationID, binding.RepositoryPath, binding.CodeHostProvider, binding.CodeHostRepository, binding.TargetBranch, issue.ID, issue.ContentDigest)
}

// localDependencyComplete requires normal lifecycle proof for the prerequisite's
// exact content in this context. Any matching claim (legacy graph-keyed rows
// can duplicate a revision) whose merge is visible at the target satisfies it.
func (s *Service) localDependencyComplete(ctx context.Context, db localScheduleStore, claims []store.LocalClaim, binding store.LocalExecutionBinding, id, targetRevision string) bool {
	var source *store.LocalExecutionIssue
	for i := range binding.Issues {
		if binding.Issues[i].ID == id {
			source = &binding.Issues[i]
			break
		}
	}
	if source == nil {
		return false
	}
	key := localClaimKey(binding, *source)
	ancestry, ok := s.worktreeManager().(interface {
		IsAncestor(context.Context, string, string, string) (bool, error)
	})
	if !ok {
		return false
	}
	for _, claim := range claims {
		if claim.RevisionKey() != key {
			continue
		}
		run, err := db.Run(ctx, claim.RunID)
		if err != nil {
			return false
		}
		status, merge := claim.TerminalStatus, claim.MergeCommitSHA
		if run != nil {
			status, merge = run.Status, run.MergeCommitSHA
		}
		if status != store.StatusComplete || !codehost.ValidCommitSHA(merge) {
			continue
		}
		if visible, err := ancestry.IsAncestor(ctx, binding.RepositoryPath, merge, targetRevision); err == nil && visible {
			return true
		}
	}
	return false
}

// recoverLocalClaimProjection finishes a lost Run-save response before ordinary
// startup diagnosis can mistake an unprojected claim for external corruption.
func (s *Service) recoverLocalClaimProjection(ctx context.Context, r config.RepositoryRegistration, runStore RunStore, run *store.Run) error {
	if run == nil || run.Stage != store.StageClaim || run.StatusCommentID != "" {
		return nil
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil || packet.LocalSource == nil {
		return nil
	}
	db, ok := runStore.(localScheduleStore)
	if !ok {
		return errors.New("local claim projection store is unavailable")
	}
	if err := s.drainPendingEffect(ctx, r, runStore, run); err != nil {
		return err
	}
	if run.StatusCommentID != "" {
		return nil
	}
	claims, err := db.LocalClaims(ctx, r.Path)
	if err != nil {
		return err
	}
	if recorder, ok := db.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, *run); err != nil {
			return err
		}
	}
	for _, claim := range claims {
		if claim.RunID == run.ID && claim.State == "claimed" {
			updated, err := s.applyStateTransition(ctx, runStore, stateTransition{Repository: registeredRepository(r), Issue: packet.Issue, Next: *run, CreateComment: true})
			if err == nil {
				*run = updated
			}
			return err
		}
	}
	return errors.New("frozen local Run has no recoverable claim receipt")
}

// projectLocalClaim exposes ordinary Run state or retained cleanup proof.
func projectLocalClaim(ctx context.Context, db localScheduleStore, item *store.LocalBacklogItem, claim store.LocalClaim) error {
	item.RunID = claim.RunID
	if claim.State == "reserved" {
		item.State, item.Reason = "claim_reserved", "claim workspace creation is reserved and will be reconciled"
		return nil
	}
	run, err := db.Run(ctx, claim.RunID)
	if err != nil {
		return err
	}
	if run == nil {
		item.State = string(claim.TerminalStatus)
		if item.State == "" {
			item.State = "missing_run"
		}
		item.Reason = "revision already consumed; retained lifecycle disposition is " + item.State
		return nil
	}
	item.PRURL = run.PullRequestURL
	item.State = string(run.Status)
	item.Reason = run.LifecycleReason
	if run.Status == store.StatusActive {
		item.State = "executing"
		if run.Stage == store.StageReady {
			item.State = "review_ready"
		}
	} else if run.Status == store.StatusWaitingForHuman {
		item.State = "awaiting_input"
	}
	return nil
}

// claimLocalIssue shares the queue admission gate with automatic polling.
func (s *Service) claimLocalIssue(ctx context.Context, r config.RepositoryRegistration, runStore RunStore, current *store.Run, number int) (IssueResult, error) {
	db, ok := runStore.(localScheduleStore)
	if !ok {
		return IssueResult{}, errors.New("store lacks local claim recovery")
	}
	if current != nil {
		return IssueResult{}, fmt.Errorf("an active Run already exists: %s", current.ID)
	}
	admissions, err := s.observeLocalQueue(ctx, r, db)
	if err != nil {
		return IssueResult{}, err
	}
	claim, reserved, err := s.freshReservation(ctx, r, db)
	if err != nil {
		return IssueResult{}, err
	}
	if reserved {
		if claim.Number != number {
			return IssueResult{}, errors.New("another local claim reserves the repository slot")
		}
		return s.resumeLocalClaim(ctx, r, db, claim)
	}
	if claim.RunID != "" {
		if admissions, err = s.observeLocalQueue(ctx, r, db); err != nil {
			return IssueResult{}, err
		}
	}
	for _, admission := range admissions {
		if admission.item.Issue.Number == number {
			result, err := s.reserveLocalAdmission(ctx, r, db, admission)
			var refusal *localAdmissionRefusal
			if errors.As(err, &refusal) {
				if recordErr := recordLocalAdmissionRefusal(ctx, db, r.Path, admission.item.Issue.ID, refusal); recordErr != nil {
					return IssueResult{}, errors.Join(err, recordErr)
				}
			}
			return result, err
		}
	}
	projection, _ := db.LocalBacklog(ctx, r.Path)
	for _, item := range projection.Items {
		if item.Issue.Number == number {
			return IssueResult{}, fmt.Errorf("local issue cannot be claimed: %s: %s", item.State, item.Reason)
		}
	}
	return IssueResult{}, errors.New("local issue is not committed and explicitly authorized")
}

// reserveLocalAdmission freezes source, policy, guidance and base before Git IO.
func (s *Service) reserveLocalAdmission(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore, admission localAdmission) (IssueResult, error) {
	r, err := s.currentLocalRegistration(r)
	if err != nil {
		return IssueResult{}, err
	}
	binding := admission.auth.Binding
	if err := s.executionBindingCurrent(r, binding); err != nil {
		return IssueResult{}, err
	}
	policy, err := s.deps.LoadRepository(r.RepositoryConfigPath)
	if err != nil {
		return IssueResult{}, err
	}
	if policy.TargetBranch != binding.TargetBranch {
		return IssueResult{}, &localAdmissionRefusal{cause: fmt.Errorf("%w: repository policy targets %q but the authorization targets %q", store.ErrLocalExecutionConflict, policy.TargetBranch, binding.TargetBranch)}
	}
	if err := harness.ValidateNativeResumeCapabilities(policy, s.deps.HarnessCapabilities); err != nil {
		return IssueResult{}, &localAdmissionRefusal{cause: err}
	}
	source := localissues.Issue{}
	for _, candidate := range admission.snapshot.Graph.Issues {
		if candidate.ID == admission.item.Issue.ID {
			source = candidate
			break
		}
	}
	if source.ID == "" || source.ContentDigest != admission.item.Issue.ContentDigest || !bytes.Equal(source.Bytes, admission.item.Issue.Bytes) {
		return IssueResult{}, &localAdmissionRefusal{cause: errors.New("claim source changed from exact admitted snapshot")}
	}
	route, err := resolveClaimRoute(source.Body, policy)
	if err != nil {
		return IssueResult{}, &localAdmissionRefusal{cause: err}
	}
	cutoff, err := s.claimCommentWatermark(ctx, registeredRepository(r), admission.item.Issue.Number)
	if err != nil {
		return IssueResult{}, err
	}
	packet := SpecificationPacket{Version: specificationPacketVersion, Issue: tracker.Issue{Number: admission.item.Issue.Number, Title: source.Title, Body: source.Body, State: "open", Labels: []string{tracker.LabelAgentReady}}, RepositoryConfig: policy, Route: route, LocalSource: &LocalSpecificationSource{CodeHostProvider: binding.CodeHostProvider, CodeHostRepository: binding.CodeHostRepository, TargetBranch: binding.TargetBranch, SchemaVersion: 1, IssueID: source.ID, Path: source.Path, ContentDigest: source.ContentDigest, GraphDigest: binding.GraphDigest, Revision: admission.snapshot.Revision, AuthorizationID: admission.auth.RequestID, InstallationID: binding.InstallationID, Bytes: source.Bytes}}
	readContext := gitadapter.Workspace{BaseSHA: admission.snapshot.Revision, Worktree: r.Path}
	packet.RepositoryGuidance, packet.RepositoryGuidancePaths, err = captureRepositoryGuidance(ctx, s.worktreeManager(), readContext)
	if err != nil {
		return IssueResult{}, err
	}
	packet.RepositoryCraft, err = captureRepositoryCraft(ctx, s.worktreeManager(), readContext, policy.RoleCraft)
	if err != nil {
		return IssueResult{}, err
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return IssueResult{}, err
	}
	key := localClaimKey(binding, admission.item.Issue)
	// The full digest is collision-checked against full durable key fields by Store.
	claim := store.LocalClaim{CodeHostProvider: binding.CodeHostProvider, CodeHostRepository: binding.CodeHostRepository, TargetBranch: binding.TargetBranch, Key: key, RepositoryPath: r.Path, InstallationID: binding.InstallationID, IssueID: source.ID, ContentDigest: source.ContentDigest, GraphDigest: binding.GraphDigest, AuthorizationID: admission.auth.RequestID, RunID: "local-" + strings.TrimPrefix(key, "sha256:"), BaseSHA: admission.snapshot.Revision, Number: packet.Issue.Number, SpecificationPacket: string(data), CreatedAt: s.deps.Now()}
	// Cutoff belongs to the reservation packet so replay cannot admit old commands.
	packet.ClaimCommentCutoff = cutoff
	data, err = json.Marshal(packet)
	if err != nil {
		return IssueResult{}, err
	}
	claim.SpecificationPacket = string(data)
	reserved, err := db.ReserveLocalClaim(ctx, claim)
	if err != nil {
		return IssueResult{}, err
	}
	return s.resumeLocalClaim(ctx, r, db, reserved)
}

// resumeLocalClaim adopts exact workspace effects before atomic Run consumption.
func (s *Service) resumeLocalClaim(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore, claim store.LocalClaim) (result IssueResult, resultErr error) {
	defer func() {
		if resultErr != nil {
			_ = recordLocalClaimFailure(context.WithoutCancel(ctx), db, r.Path, claim, resultErr)
		}
	}()
	r, err := s.currentLocalRegistration(r)
	if err != nil {
		return IssueResult{}, err
	}
	auth, err := db.LocalExecutionAuthorization(ctx, claim.AuthorizationID)
	if err != nil {
		return IssueResult{}, err
	}
	if err := s.executionBindingCurrent(r, auth.Binding); err != nil {
		return IssueResult{}, err
	}
	if err := s.localHostReady(ctx, r); err != nil {
		return IssueResult{}, fmt.Errorf("code_host_unavailable: %w", err)
	}
	exact, ok := s.worktreeManager().(gitadapter.ExactWorktreeManager)
	if !ok {
		return IssueResult{}, errors.New("Git workspace lacks exact claim recovery capability")
	}
	packet, err := decodeSpecificationPacket(claim.SpecificationPacket)
	if err != nil {
		return IssueResult{}, err
	}
	workspace, err := exact.CreateAtRevision(ctx, r.Path, claim.BaseSHA, claim.RunID)
	if err != nil {
		return IssueResult{}, err
	}
	run := store.Run{ID: claim.RunID, RepositoryPath: r.Path, IssueNumber: claim.Number, Stage: store.StageClaim, Status: store.StatusActive, Branch: workspace.Branch, Worktree: workspace.Worktree, CheckpointSHA: claim.BaseSHA, BaseCheckpointSHA: claim.BaseSHA, ImageDigest: packet.RepositoryConfig.WorkerBuild.Digest, Coordinator: s.deps.Coordinator, CheckRepairBudget: packet.RepositoryConfig.RetryLimits.CheckRepair, ReviewRepairBudget: packet.RepositoryConfig.RetryLimits.ReviewRepair, TestRevisionBudget: testRevisionBudgetForPacket(packet), ProcessedCommentID: packet.ClaimCommentCutoff, SpecificationPacket: claim.SpecificationPacket, CreatedAt: claim.CreatedAt, UpdatedAt: claim.CreatedAt}
	run, err = db.SaveLocalClaimRun(ctx, claim, run)
	if err != nil {
		return IssueResult{}, err
	}
	if recorder, ok := db.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
			return IssueResult{Run: run, Packet: packet}, err
		}
	}
	if run.StatusCommentID == "" {
		run, err = s.applyStateTransition(ctx, db, stateTransition{Repository: registeredRepository(r), Issue: packet.Issue, Next: run, CreateComment: true})
		if err != nil {
			return IssueResult{Run: run, Packet: packet}, err
		}
	}
	return IssueResult{Run: run, Packet: packet}, nil
}

// pollLocalOnce retains queue reasons and advances only explicitly eligible work.
func (s *Service) pollLocalOnce(ctx context.Context, r config.RepositoryRegistration) (PollResult, error) {
	r, err := s.currentLocalRegistration(r)
	if err != nil {
		return PollResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, r.OperationalDataPath)
	if err != nil {
		return PollResult{}, err
	}
	defer opened.Close()
	db, ok := opened.(localScheduleStore)
	if !ok {
		return PollResult{}, errors.New("store lacks local scheduling")
	}
	current, err := db.CurrentRun(ctx)
	if err != nil {
		return PollResult{}, err
	}
	if err := s.recoverLocalClaimProjection(ctx, r, db, current); err != nil {
		return PollResult{}, &pollingTransportError{err: err}
	}
	admissions, err := s.observeLocalQueue(ctx, r, db)
	if err != nil {
		return PollResult{}, err
	}
	if current != nil {
		return PollResult{Outcome: PollActiveRun, Run: *current}, nil
	}
	claim, reserved, err := s.freshReservation(ctx, r, db)
	if err != nil {
		return PollResult{}, &pollingTransportError{err: err}
	}
	if reserved {
		claimed, err := s.resumeLocalClaim(ctx, r, db, claim)
		if err != nil {
			return PollResult{}, &pollingTransportError{err: err}
		}
		return PollResult{Outcome: PollClaimed, IssueNumber: claimed.Run.IssueNumber, Run: claimed.Run}, nil
	}
	if claim.RunID != "" {
		// The released item now projects its waiting reason; admit the rest.
		if admissions, err = s.observeLocalQueue(ctx, r, db); err != nil {
			return PollResult{}, err
		}
	}
	for _, admission := range admissions {
		claimed, err := s.reserveLocalAdmission(ctx, r, db, admission)
		var refusal *localAdmissionRefusal
		if errors.As(err, &refusal) {
			if err := recordLocalAdmissionRefusal(ctx, db, r.Path, admission.item.Issue.ID, refusal); err != nil {
				return PollResult{}, &pollingTransportError{err: err}
			}
			continue
		}
		if err != nil {
			return PollResult{}, &pollingTransportError{err: err}
		}
		return PollResult{Outcome: PollClaimed, IssueNumber: claimed.Run.IssueNumber, Run: claimed.Run}, nil
	}
	return PollResult{Outcome: PollNoWork}, nil
}

// localAdmissionRefusal is a per-item policy refusal found before any claim
// effect, such as an unavailable route, a missing harness capability, or a
// target mismatch. It keeps that item waiting without stalling the queue;
// every other admission error stays a transport failure.
type localAdmissionRefusal struct{ cause error }

// Error returns the refusal cause unchanged.
func (e *localAdmissionRefusal) Error() string { return e.cause.Error() }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *localAdmissionRefusal) Unwrap() error { return e.cause }

// recordLocalAdmissionRefusal projects the refusal on its item, so the backlog
// shows why it waits instead of a stale queued state.
func recordLocalAdmissionRefusal(ctx context.Context, db localScheduleStore, repo, issueID string, refusal *localAdmissionRefusal) error {
	projection, err := db.LocalBacklog(ctx, repo)
	if err != nil {
		return err
	}
	for i := range projection.Items {
		if projection.Items[i].Issue.ID == issueID {
			item := &projection.Items[i]
			item.State, item.QueueState = "admission_refused", "admission_refused"
			item.Reason = "admission refused before any claim effect: " + refusal.Error()
			item.QueueReason = item.Reason
		}
	}
	return db.SaveLocalBacklog(ctx, repo, projection)
}

// freshReservation returns the repository's reserved claim when it may still be
// consumed. A stale reservation is released instead: its workspace is
// discarded, the row is deleted, and its authorization is invalidated, so the
// backlog shows the waiting reason. It then returns that claim with reserved
// false. Without a reservation it returns a zero claim.
func (s *Service) freshReservation(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore) (store.LocalClaim, bool, error) {
	claims, err := db.LocalClaims(ctx, r.Path)
	if err != nil {
		return store.LocalClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.State != "reserved" {
			continue
		}
		reason, err := s.staleReservationReason(ctx, r, db, claim)
		if err != nil || reason == "" {
			return claim, err == nil, err
		}
		return claim, false, s.releaseLocalClaim(ctx, r, db, claim, reason)
	}
	return store.LocalClaim{}, false, nil
}

// staleReservationReason reports why a reservation may no longer become a Run:
// its authorization was invalidated, or the committed graph is no longer the
// exact graph it reserved. An empty reason means it may resume.
func (s *Service) staleReservationReason(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore, claim store.LocalClaim) (string, error) {
	invalidations, err := db.LocalExecutionInvalidations(ctx, r.Path)
	if err != nil {
		return "", err
	}
	if reason := invalidations[claim.AuthorizationID]; reason != "" {
		return "authorization was invalidated: " + reason, nil
	}
	snapshot, err := s.refreshLocalSource(ctx, r)
	if err != nil {
		return "", err
	}
	if snapshot.Graph.Digest != claim.GraphDigest {
		return "the committed source graph changed after the reservation", nil
	}
	return "", nil
}

// releaseLocalClaim discards the reservation's workspace before its row, so an
// interruption leaves an inventoried reservation rather than an orphan branch.
func (s *Service) releaseLocalClaim(ctx context.Context, r config.RepositoryRegistration, db localScheduleStore, claim store.LocalClaim, reason string) error {
	exact, ok := s.worktreeManager().(gitadapter.ExactWorktreeManager)
	if !ok {
		return errors.New("Git workspace lacks exact claim recovery capability")
	}
	if err := exact.DiscardRevision(ctx, r.Path, claim.RunID); err != nil {
		return fmt.Errorf("discard stale claim workspace: %w", err)
	}
	if err := db.ReleaseLocalClaim(ctx, claim); err != nil {
		return err
	}
	return db.InvalidateLocalExecution(ctx, r.Path, claim.AuthorizationID, "reserved claim was released before its Run started: "+reason, s.deps.Now())
}

// recordLocalClaimFailure makes reserved Git/store ambiguity actionable without releasing its slot.
func recordLocalClaimFailure(ctx context.Context, db localScheduleStore, repo string, claim store.LocalClaim, cause error) error {
	projection, err := db.LocalBacklog(ctx, repo)
	if err != nil {
		return err
	}
	projection.Error = cause.Error()
	for i := range projection.Items {
		if projection.Items[i].Issue.ID == claim.IssueID {
			projection.Items[i].State = "claim_recovery_required"
			projection.Items[i].QueueState = "claim_recovery_required"
			projection.Items[i].Reason = cause.Error()
			projection.Items[i].QueueReason = cause.Error()
			projection.Items[i].RunID = claim.RunID
		}
	}
	return db.SaveLocalBacklog(ctx, repo, projection)
}
