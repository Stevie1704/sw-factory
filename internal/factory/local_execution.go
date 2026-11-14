package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// localExecutionStore is coordinator-owned durable preview/authorization evidence.
type localExecutionStore interface {
	RecordLocalExecutionPreview(context.Context, store.LocalExecutionPreview) (store.LocalExecutionPreview, error)
	LocalExecutionPreview(context.Context, string, string) (store.LocalExecutionPreview, error)
	LatestLocalExecutionPreview(context.Context, string) (store.LocalExecutionPreview, error)
	AuthorizeLocalExecution(context.Context, store.LocalExecutionAuthorization, *store.IdeationCommandResult) (store.LocalExecutionAuthorization, error)
	LocalExecutionAuthorization(context.Context, string) (store.LocalExecutionAuthorization, error)
	LocalExecutionAuthorizations(context.Context, string) ([]store.LocalExecutionAuthorization, error)
}

// localOperatorCommands dispatches scoped input from the one durable inbox.
type localOperatorCommands struct {
	service *Service
	store   sessionCommandStore
}

// ApplyOperatorCommand applies only explicitly declared backlog or Run semantics.
func (o localOperatorCommands) ApplyOperatorCommand(ctx context.Context, command store.IdeationCommand) (sessioncommand.Outcome, error) {
	registration, err := o.service.registration()
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	if registration.EffectiveTracker() != config.ProviderLocal {
		return sessioncommand.Outcome{Code: store.IdeationCommandUnsupported, Detail: "local operator commands require the local tracker"}, nil
	}
	switch sessioncommand.Kind(command.Kind) {
	case sessioncommand.KindPreviewExecution:
		return o.service.previewLocalExecution(ctx, o.store, registration, command)
	case sessioncommand.KindAuthorizeExecution:
		return o.service.authorizeLocalExecution(ctx, o.store, registration, command)
	case sessioncommand.KindAnswerRun, sessioncommand.KindCancelRun, sessioncommand.KindRetryRun, sessioncommand.KindResumeRun:
		return o.service.applyLocalRunCommand(ctx, registration, command)
	default:
		return sessioncommand.Outcome{Code: store.IdeationCommandUnsupported, Detail: "unsupported local operator command"}, nil
	}
}

// executionHostBinding freezes current private registration and repository policy.
func (s *Service) executionHostBinding(registration config.RepositoryRegistration) (store.LocalExecutionBinding, error) {
	repository, err := s.deps.LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		return store.LocalExecutionBinding{}, err
	}
	if registration.EffectiveTracker() != config.ProviderLocal {
		return store.LocalExecutionBinding{}, errors.New("execution authorization requires local tracker registration")
	}
	return store.LocalExecutionBinding{RepositoryPath: registration.Path, InstallationID: registration.LocalOperator.InstallationID, OperatorName: registration.LocalOperator.Name, CodeHostProvider: string(registration.EffectiveCodeHost()), CodeHostRepository: registeredRepository(registration).String(), TargetBranch: repository.TargetBranch}, nil
}

// executionBindingCurrent rejects changed host identities or target policy.
func (s *Service) executionBindingCurrent(registration config.RepositoryRegistration, binding store.LocalExecutionBinding) error {
	registration, err := s.currentLocalRegistration(registration)
	if err != nil {
		return err
	}
	current, err := s.executionHostBinding(registration)
	if err != nil {
		return err
	}
	if current.RepositoryPath != binding.RepositoryPath || current.InstallationID != binding.InstallationID || current.OperatorName != binding.OperatorName || current.CodeHostProvider != binding.CodeHostProvider || current.CodeHostRepository != binding.CodeHostRepository || current.TargetBranch != binding.TargetBranch {
		return fmt.Errorf("%w: registration or target policy changed; preview and approve again", store.ErrLocalExecutionConflict)
	}
	return nil
}

// previewLocalExecution freezes a complete fetched target graph for explicit review.
func (s *Service) previewLocalExecution(ctx context.Context, commandStore sessionCommandStore, registration config.RepositoryRegistration, command store.IdeationCommand) (sessioncommand.Outcome, error) {
	persistence, ok := commandStore.(localExecutionStore)
	if !ok {
		return sessioncommand.Outcome{}, errors.New("store does not support local execution evidence")
	}
	payload, err := sessioncommand.DecodePayload(sessioncommand.KindPreviewExecution, command.Payload)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	selected := slices.Clone(payload.(*sessioncommand.ExecutionPreviewPayload).SelectedIssueIDs)
	slices.Sort(selected)
	snapshot, err := s.refreshLocalSource(ctx, registration)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	binding, err := s.executionHostBinding(registration)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	binding.SourceKind = "target"
	binding.SourceRevision = snapshot.Revision
	binding.GraphDigest = snapshot.Graph.Digest
	binding.PublicationPolicy = "target_visible"
	binding.SelectedIssueIDs = selected
	binding.Issues = executionIssues(snapshot)
	type manifestItem struct {
		ID            string
		Number        int
		Path          string
		ContentDigest string
		Kind          localissues.Kind
	}
	type relationItem struct {
		ID, Parent   string
		Dependencies []string
	}
	manifest := make([]manifestItem, 0, len(binding.Issues))
	relations := make([]relationItem, 0, len(binding.Issues))
	for _, issue := range binding.Issues {
		manifest = append(manifest, manifestItem{issue.ID, issue.Number, issue.Path, issue.ContentDigest, issue.Kind})
		relations = append(relations, relationItem{issue.ID, issue.Parent, issue.Dependencies})
	}
	binding.Payload, err = json.Marshal(struct {
		Source, Revision, GraphDigest string
		Issues                        []manifestItem
	}{"committed target", snapshot.Revision, snapshot.Graph.Digest, manifest})
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	binding.RelationshipMapping, err = json.Marshal(relations)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	binding.PayloadSHA256 = localissues.Digest(binding.Payload)
	binding.RelationshipMappingSHA256 = localissues.Digest(binding.RelationshipMapping)
	preview, err := persistence.RecordLocalExecutionPreview(ctx, store.LocalExecutionPreview{ID: command.RequestID, Binding: binding, CreatedAt: s.deps.Now()})
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	encoded, err := json.Marshal(LocalExecutionPreviewResult{PreviewID: preview.ID, Revision: preview.Revision, GraphDigest: binding.GraphDigest, PayloadSHA256: binding.PayloadSHA256, RelationshipMappingSHA256: binding.RelationshipMappingSHA256, SelectedIssueIDs: binding.SelectedIssueIDs})
	if err != nil {
		return sessioncommand.Outcome{}, err
	}

	return sessioncommand.Outcome{Code: store.IdeationCommandApplied, SessionRevision: preview.Revision, Detail: "exact committed local issue preview; execution still requires explicit approval", Result: encoded}, nil
}

// executionIssues converts a fully validated snapshot to exact authority evidence.
func executionIssues(snapshot tracker.CommittedSnapshot) []store.LocalExecutionIssue {
	result := make([]store.LocalExecutionIssue, 0, len(snapshot.Issues))
	for _, item := range snapshot.Issues {
		source := item.Source
		result = append(result, store.LocalExecutionIssue{ID: source.ID, Number: item.Number, Path: source.Path, ContentDigest: source.ContentDigest, Bytes: source.Bytes, Kind: source.Kind, Parent: source.Parent, Dependencies: source.Dependencies})
	}
	slices.SortFunc(result, func(a, b store.LocalExecutionIssue) int { return bytes.Compare([]byte(a.ID), []byte(b.ID)) })
	return result
}

// authorizeLocalExecution revalidates fresh target source before recording authority.
func (s *Service) authorizeLocalExecution(ctx context.Context, commandStore sessionCommandStore, registration config.RepositoryRegistration, command store.IdeationCommand) (sessioncommand.Outcome, error) {
	persistence, ok := commandStore.(localExecutionStore)
	if !ok {
		return sessioncommand.Outcome{}, errors.New("store does not support local execution authority")
	}
	if existing, err := persistence.LocalExecutionAuthorization(ctx, command.RequestID); err == nil {
		return localAuthorizationOutcome(existing), nil
	} else if !errors.Is(err, store.ErrLocalExecutionNotFound) {
		return sessioncommand.Outcome{}, err
	}
	decoded, err := sessioncommand.DecodePayload(sessioncommand.KindAuthorizeExecution, command.Payload)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	payload := decoded.(*sessioncommand.ExecutionAuthorizationPayload)
	preview, err := persistence.LocalExecutionPreview(ctx, registration.Path, payload.PreviewID)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	latest, err := persistence.LatestLocalExecutionPreview(ctx, registration.Path)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	selected := slices.Clone(payload.SelectedIssueIDs)
	slices.Sort(selected)
	if latest.Revision != command.ExpectedRevision || preview.Revision != command.ExpectedRevision || preview.Binding.PayloadSHA256 != payload.PayloadSHA256 || preview.Binding.RelationshipMappingSHA256 != payload.RelationshipMappingSHA256 || preview.Binding.GraphDigest != payload.GraphDigest || !slices.Equal(preview.Binding.SelectedIssueIDs, selected) {
		return sessioncommand.Outcome{}, store.ErrLocalExecutionConflict
	}
	if err := s.executionBindingCurrent(registration, preview.Binding); err != nil {
		return sessioncommand.Outcome{}, err
	}
	snapshot, err := s.refreshLocalSource(ctx, registration)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	if snapshot.Graph.Digest != preview.Binding.GraphDigest {
		return sessioncommand.Outcome{}, fmt.Errorf("%w: committed source or relationships changed", store.ErrLocalExecutionConflict)
	}
	auth := store.LocalExecutionAuthorization{RequestID: command.RequestID, PreviewID: preview.ID, Binding: preview.Binding, CreatedAt: s.deps.Now()}
	outcome := localAuthorizationOutcome(auth)
	_, err = persistence.AuthorizeLocalExecution(ctx, auth, &store.IdeationCommandResult{RequestID: command.RequestID, Outcome: outcome.Code, Detail: outcome.Detail, SessionRevision: preview.Revision, At: s.deps.Now()})
	return outcome, err
}

// localAuthorizationOutcome never claims publication or execution has begun.
func localAuthorizationOutcome(auth store.LocalExecutionAuthorization) sessioncommand.Outcome {
	return sessioncommand.Outcome{Code: store.IdeationCommandApplied, Detail: "local execution authorized; queue remains subject to source visibility, dependencies and the active Run slot"}
}

// validateLocalStartSelection refuses an empty selection or one that names
// anything but a task of the displayed local plan. It runs before the
// approval is recorded and only reads the current preview.
func (s *Service) validateLocalStartSelection(ctx context.Context, commandStore sessionCommandStore, registration config.RepositoryRegistration, command store.IdeationCommand, selected []string) error {
	if len(selected) == 0 {
		return errors.New("approve-and-start needs at least one selected task")
	}
	planner, ok := s.deps.Tracker.(tracker.PlanPublisher)
	if !ok {
		return errors.New("local publication capability unavailable")
	}
	binding, err := (&publication.Publisher{Store: commandStore, Tracker: planner, Repository: registeredRepository(registration)}).Preview(ctx, command.SessionID)
	if err != nil {
		return err
	}
	tasks := map[string]bool{}
	for _, item := range binding.Preview.Items {
		if item.Type == string(localissues.KindTask) {
			tasks[item.Fields["local_id"]] = true
		}
	}
	for _, id := range selected {
		if !tasks[id] {
			return fmt.Errorf("approve-and-start selection %q is not a task of the displayed plan; select only tasks", id)
		}
	}
	return nil
}

// authorizeLocalPlan records authority separately after exact content approval.
func (s *Service) authorizeLocalPlan(ctx context.Context, commandStore sessionCommandStore, command store.IdeationCommand, selected []string) error {
	persistence, ok := commandStore.(localExecutionStore)
	if !ok {
		return errors.New("store does not support local execution authority")
	}
	if _, err := persistence.LocalExecutionAuthorization(ctx, command.RequestID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrLocalExecutionNotFound) {
		return err
	}
	registration, err := s.registration()
	if err != nil {
		return err
	}
	history, err := commandStore.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return err
	}
	var approval *store.PlanApproval
	for i := range history.Approvals {
		if history.Approvals[i].ID == command.RequestID {
			approval = &history.Approvals[i]
			break
		}
	}
	if approval == nil {
		return errors.New("approve-and-start requires its separate exact publication approval")
	}
	preview, err := publication.DecodePreview(approval.Payload, approval.RelationshipMapping)
	if err != nil {
		return err
	}
	if len(preview.Items) == 0 {
		return errors.New("local execution plan is empty")
	}
	planner, ok := s.deps.Tracker.(tracker.PlanPublisher)
	if !ok {
		return errors.New("local publication capability unavailable")
	}
	fresh, err := (&publication.Publisher{Store: commandStore, Tracker: planner, Repository: registeredRepository(registration)}).Preview(ctx, command.SessionID)
	if err != nil {
		return err
	}
	if !approval.Binds(fresh.PlanRevision, fresh.Target, fresh.Payload, fresh.RelationshipMapping) {
		return publication.ErrStaleApproval
	}
	binding, err := s.executionHostBinding(registration)
	if err != nil {
		return err
	}
	first := preview.Items[0].Fields
	if first["target_branch"] != binding.TargetBranch {
		return fmt.Errorf("%w: target policy changed", store.ErrLocalExecutionConflict)
	}
	binding.SourceKind = "publication"
	binding.SourceRevision = first["source_revision"]
	binding.PublicationPolicy = first["publication_policy"]
	binding.GraphDigest = first["graph_digest"]
	binding.SessionID = command.SessionID
	binding.ApprovalID = approval.ID
	binding.SelectedIssueIDs = slices.Clone(selected)
	binding.Payload = approval.Payload
	binding.RelationshipMapping = approval.RelationshipMapping
	binding.PayloadSHA256 = localissues.Digest(binding.Payload)
	binding.RelationshipMappingSHA256 = localissues.Digest(binding.RelationshipMapping)
	reader, ok := s.deps.Tracker.(tracker.CommittedIssueReader)
	if !ok {
		return errors.New("local committed source reader unavailable")
	}
	baseline, err := reader.ReadCommittedIssues(ctx, binding.SourceRevision)
	if err != nil {
		return err
	}
	binding.Issues = executionIssues(baseline)
	approved := map[string]bool{}
	for _, item := range preview.Items {
		source, err := localissues.Parse(item.Fields["path"], []byte(item.Body))
		if err != nil {
			return err
		}
		number, err := strconv.Atoi(item.Fields["number"])
		if err != nil {
			return err
		}
		if source.ID != item.Fields["local_id"] || source.ContentDigest != item.Fields["content_digest"] || item.Fields["graph_digest"] != binding.GraphDigest || item.Fields["target_branch"] != binding.TargetBranch || item.Fields["publication_policy"] != binding.PublicationPolicy {
			return errors.New("local publication preview has inconsistent authority fields")
		}
		approved[source.ID] = true
		binding.Issues = append(binding.Issues, store.LocalExecutionIssue{ID: source.ID, Number: number, Path: source.Path, ContentDigest: source.ContentDigest, Bytes: source.Bytes, Kind: source.Kind, Parent: source.Parent, Dependencies: source.Dependencies})
	}
	for _, id := range selected {
		if !approved[id] {
			return errors.New("approve-and-start selection includes an issue outside the displayed approved plan")
		}
	}
	slices.SortFunc(binding.Issues, func(a, b store.LocalExecutionIssue) int { return bytes.Compare([]byte(a.ID), []byte(b.ID)) })
	exact, err := persistence.RecordLocalExecutionPreview(ctx, store.LocalExecutionPreview{ID: command.RequestID, Binding: binding, CreatedAt: s.deps.Now()})
	if err != nil {
		return err
	}
	_, err = persistence.AuthorizeLocalExecution(ctx, store.LocalExecutionAuthorization{RequestID: command.RequestID, PreviewID: exact.ID, Binding: binding, CreatedAt: s.deps.Now()}, nil)
	return err
}

// LocalExecutionPreviewResult is the bounded command reference to retained exact bytes.
// UI reads LocalExecution to display all source bytes without duplicating a large
// graph into the durable command outcome.
type LocalExecutionPreviewResult struct {
	PreviewID                 string   `json:"preview_id"`
	Revision                  int64    `json:"revision"`
	GraphDigest               string   `json:"graph_digest"`
	PayloadSHA256             string   `json:"payload_sha256"`
	RelationshipMappingSHA256 string   `json:"relationship_mapping_sha256"`
	SelectedIssueIDs          []string `json:"selected_issue_ids"`
}
