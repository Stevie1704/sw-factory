package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// RunGateRequest selects one declared gate for the active run. An empty RunID
// addresses the repository's only active run; a gate name is always required
// so the coordinator never invents or silently changes acceptance criteria.
type RunGateRequest struct {
	// RunID optionally identifies the active run.
	RunID string
	// GateName identifies the declared gate to execute.
	GateName string
}

// BaselineRequest selects the active run whose frozen gate model should be
// evaluated before the first agent edit.
type BaselineRequest struct {
	// RunID optionally identifies the active run.
	RunID string
}

// BaselineResult reports the persisted run and every pre-edit gate outcome.
type BaselineResult struct {
	// Run is the run after baseline evaluation and any blocking transition.
	Run store.Run
	// Gates contains one result for every configured gate.
	Gates []gate.Result
}

// GateResultStore is the optional durable projection used to retain gate
// outcomes by phase and exact checkpoint without coupling the gate package to
// SQLite.
type GateResultStore interface {
	SaveGateResults(context.Context, []store.GateResult) error
	GateResults(context.Context, string, store.GatePhase, string) ([]store.GateResult, error)
}

// RunGate starts the active run's pinned worker and executes repository setup
// plus the requested gate and its declared prerequisites from the frozen
// specification packet. The resulting statuses are attached to the exact
// checkpoint SHA.
func (s *Service) RunGate(ctx context.Context, request RunGateRequest) (gate.Result, error) {
	if strings.TrimSpace(request.GateName) == "" {
		return gate.Result{}, errors.New("gate name is required")
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return gate.Result{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return gate.Result{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return gate.Result{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return gate.Result{}, err
	}
	if _, ok := declaredGate(packet, request.GateName); !ok {
		return gate.Result{}, fmt.Errorf("gate %q is not declared in the frozen specification packet", request.GateName)
	}
	if err := s.verifyGateCheckpoint(ctx, *run); err != nil {
		return gate.Result{}, err
	}
	plan, err := gateDependencyPlan(packet.RepositoryConfig.Gates, request.GateName)
	if err != nil {
		return gate.Result{}, err
	}
	suite, err := s.runGateSuite(ctx, registration, runStore, *run, packet, gate.PhaseCheckpoint, plan)
	if persistErr := persistGateSuite(ctx, runStore, *run, suite); persistErr != nil {
		err = errors.Join(err, persistErr)
	}
	for _, result := range suite.Gates {
		if result.GateName == request.GateName {
			return result, err
		}
	}
	if len(suite.Gates) == 0 {
		return gate.Result{}, err
	}
	return gate.Result{}, errors.Join(err, fmt.Errorf("gate %q was not evaluated", request.GateName))
}

// RunBaseline evaluates every frozen gate before agent edits. A blocking
// baseline failure moves the run to a visible failed preflight state unless
// the frozen issue explicitly targets each failed baseline gate.
func (s *Service) RunBaseline(ctx context.Context, request BaselineRequest) (BaselineResult, error) {
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return BaselineResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return BaselineResult{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return BaselineResult{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	if run.Stage != store.StageClaim || run.Status != store.StatusActive {
		return BaselineResult{}, fmt.Errorf("run %q must be in claim/active state for baseline, not %s/%s", run.ID, run.Stage, run.Status)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return BaselineResult{}, err
	}
	if err := s.verifyGateCheckpoint(ctx, *run); err != nil {
		return BaselineResult{}, err
	}
	suite, suiteErr := s.runGateSuite(ctx, registration, runStore, *run, packet, gate.PhaseBaseline, packet.RepositoryConfig.Gates)
	result := BaselineResult{Run: *run, Gates: suite.Gates}
	persistErr := persistGateSuite(ctx, runStore, *run, suite)
	if suiteErr == nil && persistErr == nil {
		return result, nil
	}
	if persistErr == nil && baselineFailureErrorTargeted(packet.Issue.Body, suite, suiteErr) {
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recorder.RecordEvaluationExemption(ctx, run.ID, store.EvaluationExemptionBaseline); err != nil {
				return result, fmt.Errorf("record baseline evaluation exemption: %w", err)
			}
		}
		return result, nil
	}
	effectiveErr := errors.Join(suiteErr, persistErr)

	next := *run
	next.Stage = store.StagePreflight
	next.Status = store.StatusFailed
	if suiteErr == nil && persistErr != nil {
		next.LifecycleReason = "baseline gate result persistence failure"
	} else {
		next.LifecycleReason = "baseline gate failure"
	}
	next.UpdatedAt = s.deps.Now().UTC()
	issue := packet.Issue
	updated, transitionErr := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository: commandRepository(registration),
		Issue:      issue,
		Previous:   *run,
		Next:       next,
	})
	result.Run = updated
	return result, errors.Join(fmt.Errorf("baseline is unhealthy: %w", effectiveErr), transitionErr)
}

// ensureBaselineReady requires the durable pre-edit suite before a visible
// agent can start. It checks the frozen gate set, exact checkpoint, and current
// dependency-input fingerprint so an old baseline cannot authorize new edits.
func (s *Service) ensureBaselineReady(ctx context.Context, runStore RunStore, run store.Run, packet SpecificationPacket) error {
	resultStore, ok := runStore.(GateResultStore)
	if !ok {
		return errors.New("operational store does not support baseline gate results")
	}
	fingerprint, err := setupInputFingerprint(run.Worktree, packet.RepositoryConfig.SetupFiles)
	if err != nil {
		return fmt.Errorf("fingerprint baseline setup files: %w", err)
	}
	stored, err := resultStore.GateResults(ctx, run.ID, store.GatePhaseBaseline, run.CheckpointSHA)
	if err != nil {
		return fmt.Errorf("read baseline gate results: %w", err)
	}
	if len(stored) != len(packet.RepositoryConfig.Gates) {
		return fmt.Errorf("baseline gate results are incomplete: got %d, want %d", len(stored), len(packet.RepositoryConfig.Gates))
	}
	suite := gate.SuiteResult{CheckpointSHA: run.CheckpointSHA, Phase: gate.PhaseBaseline}
	setupFailed := false
	for index, declared := range packet.RepositoryConfig.Gates {
		result := stored[index]
		if result.RunID != run.ID || result.CheckpointSHA != run.CheckpointSHA || result.Phase != store.GatePhaseBaseline || result.GateName != declared.Name || result.Blocking != declared.Blocking || result.SetupFingerprint != fingerprint {
			return fmt.Errorf("baseline gate result %q does not match the frozen run identity", declared.Name)
		}
		outcome := gate.Outcome(result.Outcome)
		suite.Gates = append(suite.Gates, gate.Result{
			CheckpointSHA:    result.CheckpointSHA,
			GateName:         result.GateName,
			Phase:            gate.PhaseBaseline,
			Blocking:         result.Blocking,
			SetupFingerprint: result.SetupFingerprint,
			Outcome:          outcome,
		})
		switch outcome {
		case gate.OutcomePassed, gate.OutcomeFailed, gate.OutcomeError, gate.OutcomeSkipped, gate.OutcomeSetupFailed:
		default:
			return fmt.Errorf("baseline gate result %q has unsupported outcome %q", declared.Name, outcome)
		}
		if outcome == gate.OutcomeSetupFailed {
			setupFailed = true
		}
	}
	if setupFailed && !baselineSetupTargeted(packet.Issue.Body) {
		return errors.New("baseline setup failed and is not explicitly targeted by the issue")
	}
	if suiteErrHasBlockingFailure(suite) && !baselineFailureTargeted(packet.Issue.Body, suite) {
		return errors.New("baseline has an untargeted blocking gate failure")
	}
	return nil
}

// verifyGateCheckpoint refuses to evaluate a mutable or dirty worktree under
// a stale checkpoint identity.
func (s *Service) verifyGateCheckpoint(ctx context.Context, run store.Run) error {
	var inspector gitadapter.WorktreeInspector
	if s.deps.GitWorkspace != nil {
		inspector = s.deps.GitWorkspace
	} else if candidate, ok := s.deps.Worktree.(gitadapter.WorktreeInspector); ok {
		inspector = candidate
	}
	if inspector == nil {
		return errors.New("a GitWorkspace inspector is required before running gates")
	}
	state, err := inspector.Inspect(ctx, run.Worktree)
	if err != nil {
		return fmt.Errorf("inspect gate worktree: %w", err)
	}
	if state.HeadSHA != run.CheckpointSHA {
		return fmt.Errorf("worktree HEAD %q does not match gate checkpoint %q", state.HeadSHA, run.CheckpointSHA)
	}
	if len(state.ChangedPaths) != 0 {
		return errors.New("worktree has uncommitted changes; checkpoint it before running gates")
	}
	return nil
}

// runGateSuite starts the pinned gate worker and evaluates one frozen ordered
// gate suite with setup shared across every gate.
func (s *Service) runGateSuite(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, packet SpecificationPacket, phase gate.Phase, gates []config.GateConfig) (gate.SuiteResult, error) {
	if run.ImageDigest != packet.RepositoryConfig.WorkerBuild.Digest {
		return gate.SuiteResult{}, fmt.Errorf("run %q image digest %q differs from frozen packet digest %q", run.ID, run.ImageDigest, packet.RepositoryConfig.WorkerBuild.Digest)
	}
	if s.deps.CommitStatuses == nil {
		return gate.SuiteResult{}, errors.New("GitHub client does not support Commit Statuses")
	}
	fingerprint, err := setupInputFingerprint(run.Worktree, packet.RepositoryConfig.SetupFiles)
	if err != nil {
		return gate.SuiteResult{}, fmt.Errorf("fingerprint configured setup files: %w", err)
	}
	gitMetadataPath, err := prepareGitMetadataProjection(run.ID, registration.Path, run.Worktree)
	if err != nil {
		return gate.SuiteResult{}, fmt.Errorf("prepare worker git metadata: %w", err)
	}
	if err := s.deps.Worker.Start(ctx, worker.StartRequest{
		RunID:           run.ID,
		WorktreePath:    run.Worktree,
		GitMetadataPath: gitMetadataPath,
		Image:           packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:     run.ImageDigest,
		Caches:          workerCaches(packet.RepositoryConfig.Caches),
		Role:            "gate",
	}); err != nil {
		return gate.SuiteResult{}, err
	}
	skipSetup := setupAlreadySucceeded(ctx, runStore, run, phase, fingerprint)
	return (gate.Runner{
		Runtime:    s.deps.Worker,
		Repository: github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository},
		Statuses:   s.deps.CommitStatuses,
	}).RunSuite(ctx, gate.SuiteRequest{
		RunID:                  run.ID,
		CheckpointSHA:          run.CheckpointSHA,
		Setup:                  packet.RepositoryConfig.Setup,
		SetupEnvironmentPolicy: packet.RepositoryConfig.SetupEnvironmentPolicy,
		SetupTimeout:           packet.RepositoryConfig.Timeouts.Setup,
		Gates:                  gates,
		Phase:                  phase,
		SetupFingerprint:       fingerprint,
		SkipSetup:              skipSetup,
	})
}

// setupAlreadySucceeded reports whether durable results prove setup completed
// for this exact run, phase, checkpoint, and configured dependency fingerprint.
// An empty fingerprint deliberately disables reuse because no dependency-input
// contract exists for that configuration.
func setupAlreadySucceeded(ctx context.Context, runStore RunStore, run store.Run, phase gate.Phase, fingerprint string) bool {
	if runStore == nil || fingerprint == "" {
		return false
	}
	resultStore, ok := runStore.(GateResultStore)
	if !ok {
		return false
	}
	results, err := resultStore.GateResults(ctx, run.ID, store.GatePhase(phase), run.CheckpointSHA)
	if err != nil || len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.SetupFingerprint != fingerprint || result.Outcome == store.GateOutcomeSetupFailed {
			return false
		}
	}
	return true
}

// persistGateSuite records content-limited gate projections when the selected
// operational store supports the issue #9 result table.
func persistGateSuite(ctx context.Context, runStore RunStore, run store.Run, suite gate.SuiteResult) error {
	resultStore, ok := runStore.(GateResultStore)
	if !ok || len(suite.Gates) == 0 {
		return nil
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.EnsureEvaluationSummary(ctx, run); err != nil {
			return fmt.Errorf("ensure local evaluation summary: %w", err)
		}
	}
	results := make([]store.GateResult, 0, len(suite.Gates))
	for ordinal, result := range suite.Gates {
		results = append(results, store.GateResult{
			RunID:            run.ID,
			CheckpointSHA:    result.CheckpointSHA,
			Phase:            store.GatePhase(result.Phase),
			Ordinal:          ordinal,
			GateName:         result.GateName,
			Outcome:          store.GateOutcome(result.Outcome),
			Status:           string(result.Status.State),
			Blocking:         result.Blocking,
			SkipReason:       result.SkipReason,
			SetupFingerprint: result.SetupFingerprint,
		})
	}
	if err := resultStore.SaveGateResults(ctx, results); err != nil {
		return err
	}
	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recorder.RecordEvaluationGateRun(ctx, run.ID, len(suite.Gates)); err != nil {
			return fmt.Errorf("record local gate run: %w", err)
		}
		if err := recorder.RefreshEvaluationCounts(ctx, run.ID); err != nil {
			return fmt.Errorf("refresh local evaluation counts: %w", err)
		}
		for _, result := range suite.Gates {
			var category store.EvaluationBlockerCategory
			switch result.Outcome {
			case gate.OutcomeSetupFailed:
				category = store.EvaluationBlockerSetup
			case gate.OutcomeFailed, gate.OutcomeError, gate.OutcomeSkipped:
				category = store.EvaluationBlockerGate
			default:
				continue
			}
			if err := recorder.RecordEvaluationBlocker(ctx, run.ID, category); err != nil {
				return fmt.Errorf("record local evaluation blocker: %w", err)
			}
		}
	}
	return nil
}

// setupInputFingerprint hashes the configured manifest and lockfile contents
// so each suite records which dependency graph setup evaluated.
func setupInputFingerprint(worktree string, files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	hasher := sha256.New()
	for _, relative := range files {
		path := filepath.Join(worktree, filepath.FromSlash(relative))
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", relative, err)
		}
		_, _ = hasher.Write([]byte(relative))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(data)
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// baselineFailureTargeted recognizes the coordinator-owned issue marker for an
// explicitly accepted pre-existing gate failure. The marker is intentionally
// exact so ordinary issue prose cannot silently bypass the baseline boundary.
func baselineFailureTargeted(body string, suite gate.SuiteResult) bool {
	body = strings.ToLower(body)
	for _, result := range suite.Gates {
		if result.Outcome != gate.OutcomeFailed && result.Outcome != gate.OutcomeError && result.Outcome != gate.OutcomeSkipped && result.Outcome != gate.OutcomeSetupFailed {
			continue
		}
		if !result.Blocking {
			continue
		}
		if !strings.Contains(body, "<!-- factory-baseline-target: all -->") && !strings.Contains(body, "<!-- factory-baseline-target: "+strings.ToLower(result.GateName)+" -->") && !(result.Outcome == gate.OutcomeSetupFailed && strings.Contains(body, "<!-- factory-baseline-target: setup -->")) {
			return false
		}
	}
	return suiteErrHasBlockingFailure(suite)
}

// baselineSetupTargeted reports whether the frozen issue explicitly accepts a
// pre-existing setup failure, including the all-failures shorthand.
func baselineSetupTargeted(body string) bool {
	body = strings.ToLower(body)
	return strings.Contains(body, "<!-- factory-baseline-target: all -->") || strings.Contains(body, "<!-- factory-baseline-target: setup -->")
}

// baselineFailureErrorTargeted ensures an issue marker suppresses only the
// declared baseline failure, never status publication or other infrastructure
// errors that happen to accompany it.
func baselineFailureErrorTargeted(body string, suite gate.SuiteResult, suiteErr error) bool {
	if suiteErr == nil || (!baselineFailureTargeted(body, suite) && !baselineSetupFailureTargeted(body, suite)) {
		return false
	}
	var failure *gate.SuiteFailure
	if !errors.As(suiteErr, &failure) {
		return false
	}
	for _, cause := range failure.Failures {
		var gateFailure *gate.GateFailure
		var setupFailure *gate.SetupFailure
		var dependencyFailure *gate.DependencyFailure
		if errors.As(cause, &gateFailure) || errors.As(cause, &setupFailure) || errors.As(cause, &dependencyFailure) {
			continue
		}
		return false
	}
	return true
}

// baselineSetupFailureTargeted recognizes a setup-only exemption when setup
// prevented every declared gate from running, including all-advisory suites.
func baselineSetupFailureTargeted(body string, suite gate.SuiteResult) bool {
	for _, result := range suite.Gates {
		if result.Outcome == gate.OutcomeSetupFailed {
			return baselineSetupTargeted(body)
		}
	}
	return false
}

// suiteErrHasBlockingFailure reports whether the suite contains a blocking
// result that required baseline disposition.
func suiteErrHasBlockingFailure(suite gate.SuiteResult) bool {
	for _, result := range suite.Gates {
		if result.Blocking && (result.Outcome == gate.OutcomeFailed || result.Outcome == gate.OutcomeError || result.Outcome == gate.OutcomeSkipped || result.Outcome == gate.OutcomeSetupFailed) {
			return true
		}
	}
	return false
}

// prepareGitMetadataProjection creates or reuses a sanitized Git metadata
// projection for a run. It copies history and refs without copying repository
// configuration, remotes, hooks, or other files that could carry credentials or
// host paths. The projection is stable for the run so a reused worker keeps the
// same mounted metadata after a coordinator restart. It returns the projection
// path or an error if the inputs or projection are invalid.
func prepareGitMetadataProjection(runID, repositoryPath, worktreePath string) (string, error) {
	if strings.TrimSpace(runID) == "" || filepath.Base(runID) != runID || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return "", fmt.Errorf("run id %q cannot name a git metadata projection", runID)
	}
	if !filepath.IsAbs(worktreePath) {
		return "", errors.New("worktree path must be absolute for a git metadata projection")
	}
	source := filepath.Join(repositoryPath, ".git")
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("inspect repository git metadata: %w", err)
	}
	if !sourceInfo.IsDir() {
		return "", errors.New("repository git metadata must be a directory")
	}

	projectionParent := filepath.Join(filepath.Dir(worktreePath), ".factory-git")
	projection := filepath.Join(projectionParent, runID)
	if projectionInfo, statErr := os.Stat(projection); statErr == nil {
		if !projectionInfo.IsDir() {
			return "", fmt.Errorf("git metadata projection %q is not a directory", projection)
		}
		if _, statErr := os.Stat(filepath.Join(projection, "HEAD")); statErr != nil {
			return "", fmt.Errorf("git metadata projection %q is incomplete: %w", projection, statErr)
		}
		if _, statErr := os.Stat(filepath.Join(projection, "config")); statErr == nil {
			return "", fmt.Errorf("git metadata projection %q contains forbidden configuration", projection)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect git metadata projection configuration: %w", statErr)
		}
		if err := overlayWorktreeGitState(worktreePath, projection); err != nil {
			return "", fmt.Errorf("refresh git metadata projection: %w", err)
		}
		if err := makeGitProjectionReadable(projection); err != nil {
			return "", fmt.Errorf("prepare git metadata projection permissions: %w", err)
		}
		return projection, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect git metadata projection: %w", statErr)
	}
	if err := os.MkdirAll(projectionParent, 0o700); err != nil {
		return "", fmt.Errorf("create git metadata projection parent: %w", err)
	}
	temporary, err := os.MkdirTemp(projectionParent, "."+runID+".git-projection-")
	if err != nil {
		return "", fmt.Errorf("create temporary git metadata projection: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := copyGitMetadata(source, temporary); err != nil {
		return "", fmt.Errorf("copy git metadata: %w", err)
	}
	if err := overlayWorktreeGitState(worktreePath, temporary); err != nil {
		return "", fmt.Errorf("overlay worktree git state: %w", err)
	}
	if err := makeGitProjectionReadable(temporary); err != nil {
		return "", fmt.Errorf("prepare git metadata projection permissions: %w", err)
	}
	if err := os.Rename(temporary, projection); err != nil {
		return "", fmt.Errorf("publish git metadata projection: %w", err)
	}
	return projection, nil
}

// copyGitMetadata copies permitted regular Git metadata from source to
// destination while omitting local configuration and files that can define
// remotes, commands, or host paths. It rejects symbolic links and other
// non-regular entries.
func copyGitMetadata(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if skipGitMetadata(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", relative)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return copyGitMetadataFile(path, target, 0o640)
	})
}

// makeGitProjectionReadable gives the fixed worker identity read and execute
// access through the projection's host group without granting world access or
// write access.
func makeGitProjectionReadable(projection string) error {
	return filepath.WalkDir(projection, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", path)
		}
		mode := gitProjectionPermissions(info.Mode())
		return os.Chmod(path, mode)
	})
}

// gitProjectionPermissions returns owner-plus-group permissions for one
// projected Git metadata entry. The worker receives the projection's host
// group at Docker start, so no world-readable or world-executable bits are
// needed.
func gitProjectionPermissions(mode fs.FileMode) fs.FileMode {
	permissions := mode.Perm() & 0o700
	if mode.IsDir() {
		return permissions | 0o050
	}
	permissions |= 0o040
	if mode.Perm()&0o111 != 0 {
		permissions |= 0o010
	}
	return permissions
}

// skipGitMetadata reports whether a relative Git metadata path should be
// excluded from a projection because it can contain configuration, credentials,
// executable hooks, or host-specific indirection.
func skipGitMetadata(relative string, directory bool) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch part {
		case "hooks", "modules", "remotes", "branches", "worktrees":
			return true
		}
	}
	if directory {
		return false
	}
	base := filepath.Base(relative)
	return base == "config" || base == "config.worktree" || base == "FETCH_HEAD" || base == "alternates"
}

// copyGitMetadataFile streams one regular Git metadata file into the projection
// while preserving the specified non-secret permission bits.
func copyGitMetadataFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	if err := os.Chmod(destination, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	chmodErr := output.Chmod(mode)
	closeErr := output.Close()
	return errors.Join(copyErr, chmodErr, closeErr)
}

// overlayWorktreeGitState copies the run worktree's HEAD and index from its
// private worktree admin directory onto the common projected metadata when
// those files are available.
func overlayWorktreeGitState(worktreePath, projection string) error {
	pointer, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	line := strings.TrimSpace(strings.SplitN(string(pointer), "\n", 2)[0])
	if !strings.HasPrefix(line, "gitdir:") {
		return nil
	}
	adminPath := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if adminPath == "" {
		return errors.New("worktree git pointer has no admin path")
	}
	if !filepath.IsAbs(adminPath) {
		adminPath = filepath.Join(worktreePath, adminPath)
	}
	for _, name := range []string{"HEAD", "index"} {
		source := filepath.Join(adminPath, name)
		info, statErr := os.Stat(source)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("worktree git state %q is not a regular file", name)
		}
		if err := copyGitMetadataFile(source, filepath.Join(projection, name), 0o640); err != nil {
			return err
		}
	}
	return nil
}

// decodeSpecificationPacket decodes and validates a serialized frozen specification packet.
// It returns an error when the packet is missing, malformed, or uses an unsupported version.
func decodeSpecificationPacket(serialized string) (SpecificationPacket, error) {
	if strings.TrimSpace(serialized) == "" {
		return SpecificationPacket{}, errors.New("active run has no frozen specification packet")
	}
	var packet SpecificationPacket
	if err := json.Unmarshal([]byte(serialized), &packet); err != nil {
		return SpecificationPacket{}, fmt.Errorf("decode frozen specification packet: %w", err)
	}
	if packet.Version != specificationPacketVersion {
		return SpecificationPacket{}, fmt.Errorf("unsupported frozen specification packet version %d", packet.Version)
	}
	return packet, nil
}

// declaredGate finds a gate with the specified name in the frozen configuration.
// It returns the matching gate and true, or an empty gate configuration and false
// when no match exists.
func declaredGate(packet SpecificationPacket, name string) (config.GateConfig, bool) {
	for _, candidate := range packet.RepositoryConfig.Gates {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return config.GateConfig{}, false
}

// gateDependencyPlan returns the requested gate and every declared prerequisite
// in repository order, preserving the ordered dependency contract for the
// single-gate compatibility API.
func gateDependencyPlan(gates []config.GateConfig, target string) ([]config.GateConfig, error) {
	required := map[string]bool{target: true}
	for changed := true; changed; {
		changed = false
		for _, candidate := range gates {
			if !required[candidate.Name] {
				continue
			}
			for _, dependency := range candidate.DependsOn {
				if !required[dependency] {
					required[dependency] = true
					changed = true
				}
			}
		}
	}
	plan := make([]config.GateConfig, 0, len(required))
	for _, candidate := range gates {
		if required[candidate.Name] {
			plan = append(plan, candidate)
		}
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("gate %q is not declared in the frozen specification packet", target)
	}
	return plan, nil
}

// workerCaches converts repository cache configurations into worker cache mounts.
func workerCaches(caches []config.CacheConfig) []worker.CacheMount {
	result := make([]worker.CacheMount, 0, len(caches))
	for _, cache := range caches {
		result = append(result, worker.CacheMount{Name: cache.Name, HostPath: cache.Path, ReadOnly: cache.ReadOnly})
	}
	return result
}
