package factory

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// LocalSpecificationSource is immutable local source/provenance in a Run packet.
type LocalSpecificationSource struct {
	CodeHostProvider, CodeHostRepository, TargetBranch                                   string
	SchemaVersion                                                                        int `json:"schema_version"`
	IssueID, Path, ContentDigest, GraphDigest, Revision, AuthorizationID, InstallationID string
	Bytes                                                                                []byte
}

// refreshLocalSource uses current policy for every coordinator observation,
// including fresh preview, authorization and queue admission.
func (s *Service) refreshLocalSource(ctx context.Context, registration config.RepositoryRegistration) (tracker.CommittedSnapshot, error) {
	policy, err := s.deps.LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		return tracker.CommittedSnapshot{}, err
	}
	reader, ok := s.deps.Tracker.(tracker.TargetCommittedIssueReader)
	if !ok {
		return tracker.CommittedSnapshot{}, errors.New("tracker cannot observe the current target branch; recompose the supported local adapter")
	}
	return reader.RefreshCommittedIssuesAt(ctx, registration.Path, policy.TargetBranch)
}

// runIssue reads mutable lifecycle state while preserving a local frozen source.
func (s *Service) runIssue(ctx context.Context, repository tracker.Repository, run store.Run) (tracker.Issue, error) {
	registration, err := s.registration()
	if err != nil {
		return tracker.Issue{}, err
	}
	if err := validateRunAdapter(registration, run); err != nil {
		return tracker.Issue{}, err
	}
	return frozenRunIssue(ctx, s.deps.Tracker, repository, run)
}

// validateRunAdapter prevents a selector edit from reinterpreting persisted
// tracker numbers or replaying their effects through another provider.
func validateRunAdapter(r config.RepositoryRegistration, run store.Run) error {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		if r.EffectiveTracker() != config.ProviderLocal {
			return nil
		}
		return fmt.Errorf("cannot prove Run %s tracker binding; restore its original tracker registration before recovery", run.ID)
	}
	local := r.EffectiveTracker() == config.ProviderLocal
	if local != (packet.LocalSource != nil) {
		return fmt.Errorf("Run %s belongs to another tracker; restore its original tracker registration before replay or commands", run.ID)
	}
	if packet.LocalSource != nil {
		source := packet.LocalSource
		if source.SchemaVersion != 1 || source.InstallationID != r.LocalOperator.InstallationID || source.CodeHostProvider != string(r.EffectiveCodeHost()) || source.CodeHostRepository != registeredRepository(r).String() || run.RepositoryPath != r.Path {
			return fmt.Errorf("Run %s local installation/code-host binding changed; restore its original registration before recovery", run.ID)
		}
	}
	return nil
}

// runAdapterMismatch checks only retained active state before doctor would
// contact a different tracker. Store readiness owns missing/unreadable stores.
func (s *Service) runAdapterMismatch(ctx context.Context, r config.RepositoryRegistration) error {
	if r.OperationalDataPath == "" {
		return nil
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, r.OperationalDataPath)
	if err != nil {
		return nil
	}
	defer opened.Close()
	run, err := opened.CurrentRun(ctx)
	if err != nil || run == nil {
		return nil
	}
	return validateRunAdapter(r, *run)
}

// frozenRunIssue is also used by read-only recovery inspection.
func frozenRunIssue(ctx context.Context, client tracker.Client, repository tracker.Repository, run store.Run) (tracker.Issue, error) {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err == nil && packet.LocalSource != nil {
		reader, ok := client.(tracker.IssueProjectionReader)
		if !ok {
			return tracker.Issue{}, errors.New("local mutable issue projection is unavailable")
		}
		return reader.ProjectIssue(ctx, repository, packet.Issue)
	}
	if _, local := client.(tracker.IssueProjectionReader); local {
		return tracker.Issue{}, errors.New("remote Run cannot be projected through a local tracker; restore original registration")
	}
	return client.Issue(ctx, repository, run.IssueNumber)
}

// generatedRemoteReference detects remote work-item references in generated
// local PR text. Unsafe advisory text is omitted, never rewritten as authority.
var generatedRemoteReference = regexp.MustCompile(`(?i)(?:#[0-9]+|https?://[^\s]+/issues/[0-9]+)`)

// localPullRequestTitle avoids giving a local integer remote issue semantics.
func localPullRequestTitle(packet SpecificationPacket) string {
	if packet.LocalSource == nil {
		return defaultString(packet.Issue.Title, fmt.Sprintf("Issue #%d", packet.Issue.Number))
	}
	if packet.Issue.Title != "" && !generatedRemoteReference.MatchString(packet.Issue.Title) {
		return packet.Issue.Title
	}
	return "Implement " + packet.LocalSource.IssueID
}

// generatedLocalPullRequestBody links frozen evidence without copying untrusted
// source prose into a code host's implicit issue-closing syntax. Human-owned PR
// sections remain preserved by the ordinary merge function.
func generatedLocalPullRequestBody(run store.Run, packet SpecificationPacket, gates []gate.Result) string {
	source := packet.LocalSource
	return fmt.Sprintf("%s\n## Factory run\n\n- run: `%s`\n- local issue: `%s`\n- accepted source: `%s` at `%s`\n- content identity: `%s`\n- graph identity: `%s`\n- specification packet: version %d, target branch `%s`\n- checkpoint: `%s`\n- stage: `draft_pr`\n- local Factory UI path: `/issues/%s`\n- deterministic Gate results in this pass: %d\n\nRead the accepted source and structured Gate/review evidence in Factory before human merge. Local tracker numbers carry no remote issue-closing authority.\n\n### Control commands\n\n- `factory status`\n- `factory draft-pr --run-id %s`\n\n%s", generatedPullRequestStart, run.ID, source.IssueID, source.Path, source.Revision, source.ContentDigest, source.GraphDigest, packet.Version, packet.RepositoryConfig.TargetBranch, run.CheckpointSHA, source.IssueID, len(gates), run.ID, generatedPullRequestEnd)
}
