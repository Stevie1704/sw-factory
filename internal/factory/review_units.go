package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/reviewunits"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	reviewRoundDirectoryName = "review-round"
	reviewRoundArtifactName  = "review.diff"
	reviewUnitArtifactName   = "review-unit.diff"
)

// reviewRoundID derives a stable round identity from the run and exact diff
// identity. A changed checkpoint or artifact can never reuse its manifest.
func reviewRoundID(run store.Run, diffSHA256 string) string {
	identity := run.ID + "\x00" + reviewDiffBase(run) + "\x00" + run.CheckpointSHA + "\x00" + diffSHA256
	digest := sha256.Sum256([]byte(identity))
	return "rr-" + hex.EncodeToString(digest[:8])
}

// safeLaunchIdentifier validates the narrow identifier vocabulary shared by
// invocation packet and report identities.
func safeLaunchIdentifier(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

// nextReviewUnit selects the lowest ordered uncompleted unit for one axis. A
// requested unit is accepted only when it is part of the frozen manifest and
// has no accepted result or active invocation.
func nextReviewUnit(ctx context.Context, rounds store.ReviewRoundStore, run store.Run, round store.ReviewRound, role, requested string, active []ActiveLaunchObservation) (*store.ReviewUnit, error) {
	units, err := rounds.ReviewUnits(ctx, round.ID)
	if err != nil {
		return nil, fmt.Errorf("read review units for %s: %w", role, err)
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("review round %q has no persisted units", round.ID)
	}
	results, err := rounds.ReviewUnitResults(ctx, round.ID, role)
	if err != nil {
		return nil, fmt.Errorf("read %s review unit results: %w", role, err)
	}
	completed := make(map[string]struct{}, len(results))
	for _, result := range results {
		completed[result.UnitID] = struct{}{}
	}
	activeUnits := make(map[string]struct{})
	for _, observation := range active {
		invocation := observation.Invocation
		if invocation.Role == role && invocation.ReviewRoundID == round.ID && invocation.ReviewUnitID != "" {
			activeUnits[invocation.ReviewUnitID] = struct{}{}
		}
	}
	if requested != "" {
		for index := range units {
			if units[index].UnitID != requested {
				continue
			}
			if _, exists := completed[requested]; exists {
				return nil, fmt.Errorf("review unit %q already has a %s result", requested, role)
			}
			if _, exists := activeUnits[requested]; exists {
				return nil, fmt.Errorf("review unit %q already has an active %s invocation", requested, role)
			}
			return &units[index], nil
		}
		return nil, fmt.Errorf("review unit %q is not in round %q", requested, round.ID)
	}
	for index := range units {
		if _, exists := completed[units[index].UnitID]; exists {
			continue
		}
		if _, exists := activeUnits[units[index].UnitID]; exists {
			continue
		}
		return &units[index], nil
	}
	return nil, fmt.Errorf("all %s review units are already complete or active", role)
}

// reviewContextWithUnit extends an isolated review context with the shared
// manifest identity and ownership ranges for one fresh invocation.
func reviewContextWithUnit(base prompt.ReviewContext, round store.ReviewRound, unit store.ReviewUnit, unitCount int) *prompt.ReviewContext {
	base.ReviewRoundID = round.ID
	base.ReviewUnitID = unit.UnitID
	base.ReviewUnitOrdinal = unit.Ordinal
	base.ReviewUnitCount = unitCount
	base.ReviewUnitWorkloadBytes = unit.WorkloadBytes
	base.ReviewUnitDiffSHA256 = unit.DiffSHA256
	base.ReviewManifestSHA256 = round.ManifestSHA256
	base.ReviewPrimaryRanges = append([]store.ReviewUnitRange(nil), unit.PrimaryRanges...)
	base.ReviewContextRanges = append([]store.ReviewUnitRange(nil), unit.ContextRanges...)
	base.ReviewPrimaryNonTextFiles = append([]string(nil), unit.PrimaryNonTextFiles...)
	return &base
}

// preparePartitionedReview creates or validates the immutable round manifest,
// selects one axis unit, and writes the bounded worker artifact before packet
// publication. It is used only by stores that expose the normalized projection;
// historical compatibility stores retain the whole-diff path.
func (l *invocationLifecycle) preparePartitionedReview(ctx context.Context, rounds store.ReviewRoundStore, run store.Run, invocation store.Invocation, base prompt.ReviewContext, requested string, registration config.RepositoryRegistration) (*prompt.ReviewContext, string, string, error) {
	sourcePath := filepath.Join(invocation.InvocationDirectory, reviewDiffFileName)
	metadata, err := inspectReviewArtifact(sourcePath)
	if err != nil {
		return nil, "", "", err
	}
	if metadata.bytes < 0 {
		return nil, "", "", errors.New("review diff has a negative byte count")
	}
	hostPolicy := config.EffectiveReviewHostConfig(registration.Review)
	round, err := rounds.ReviewRound(ctx, run.ID, run.CheckpointSHA)
	if err != nil {
		return nil, "", "", fmt.Errorf("read review round: %w", err)
	}
	units := []store.ReviewUnit(nil)
	if round == nil {
		data, readErr := os.ReadFile(sourcePath)
		if readErr != nil {
			return nil, "", "", fmt.Errorf("read exact review diff for partitioning: %w", readErr)
		}
		policy := config.EffectiveReviewUnitConfig(runReviewUnitConfig(run))
		manifest, buildErr := reviewunits.Build(data, run.ID, reviewDiffBase(run), run.CheckpointSHA, reviewunits.Policy{SchemaVersion: reviewunits.SchemaVersion, PolicyVersion: reviewunits.PolicyVersion, MaxUnitBytes: policy.MaxUnitBytes, MaxUnits: policy.MaxUnits, ContextLines: reviewDiffContextLines})
		if buildErr != nil {
			return nil, "", "", fmt.Errorf("partition exact review diff: %w", buildErr)
		}
		roundID := reviewRoundID(run, manifest.DiffSHA256)
		canonicalPath := filepath.Join(filepath.Dir(run.Worktree), ".factory-agents", run.ID, reviewRoundDirectoryName, reviewRoundArtifactName)
		if copyErr := copyReviewArtifact(sourcePath, canonicalPath); copyErr != nil {
			return nil, "", "", fmt.Errorf("persist canonical review diff: %w", copyErr)
		}
		roundValue := store.ReviewRound{
			ID:                 roundID,
			RunID:              run.ID,
			BaseCheckpointSHA:  reviewDiffBase(run),
			CheckpointSHA:      run.CheckpointSHA,
			DiffPath:           canonicalPath,
			DiffBytes:          int64(manifest.DiffBytes),
			DiffSHA256:         manifest.DiffSHA256,
			ManifestSHA256:     manifest.ManifestSHA256,
			SchemaVersion:      manifest.SchemaVersion,
			PolicyVersion:      manifest.PolicyVersion,
			MaxUnitBytes:       manifest.MaxUnitBytes,
			MaxUnits:           manifest.MaxUnits,
			ContextLines:       manifest.ContextLines,
			Concurrency:        hostPolicy.Concurrency,
			AuthorizedMaxUnits: 0,
			Status:             store.ReviewRoundStatusPending,
		}
		units = reviewUnitsFromManifest(roundID, manifest)
		if atomic, atomicOK := rounds.(store.ReviewManifestStore); atomicOK {
			if err := atomic.SaveReviewManifest(ctx, roundValue, units); err != nil {
				return nil, "", "", fmt.Errorf("persist review manifest: %w", err)
			}
		} else {
			if err := rounds.SaveReviewRound(ctx, roundValue); err != nil {
				return nil, "", "", fmt.Errorf("persist review round: %w", err)
			}
			if err := rounds.SaveReviewUnits(ctx, roundID, units); err != nil {
				return nil, "", "", fmt.Errorf("persist review unit manifest: %w", err)
			}
		}
		round = &roundValue
	} else {
		if round.SchemaVersion != reviewunits.SchemaVersion || round.PolicyVersion != reviewunits.PolicyVersion {
			return nil, "", "", fmt.Errorf("persisted review round uses unsupported partition policy %q/%d", round.PolicyVersion, round.SchemaVersion)
		}
		if round.CheckpointSHA != run.CheckpointSHA || round.BaseCheckpointSHA != reviewDiffBase(run) || round.DiffBytes != metadata.bytes || round.DiffSHA256 != metadata.sha256 {
			return nil, "", "", errors.New("persisted review round does not match the exact checkpoint diff")
		}
		if err := validateReviewArtifact(round.DiffPath, round.DiffBytes, round.DiffSHA256); err != nil {
			return nil, "", "", fmt.Errorf("validate canonical review diff: %w", err)
		}
	}
	units, err = validatePersistedReviewManifest(ctx, rounds, run, *round)
	if err != nil {
		return nil, "", "", err
	}
	if len(units) > hostPolicy.AuthorizedUnits {
		if round.Status != store.ReviewRoundStatusAwaitingAuthorization {
			if updateErr := rounds.UpdateReviewRoundStatus(ctx, round.ID, store.ReviewRoundStatusAwaitingAuthorization); updateErr != nil {
				return nil, "", "", updateErr
			}
		}
		return nil, "", "", fmt.Errorf("review round requires %d units, above host authorization ceiling %d; human disposition is required", len(units), hostPolicy.AuthorizedUnits)
	}
	allowedUnits := round.MaxUnits
	if round.AuthorizedMaxUnits > allowedUnits {
		allowedUnits = round.AuthorizedMaxUnits
	}
	if len(units) > allowedUnits {
		if round.Status != store.ReviewRoundStatusAwaitingAuthorization {
			if updateErr := rounds.UpdateReviewRoundStatus(ctx, round.ID, store.ReviewRoundStatusAwaitingAuthorization); updateErr != nil {
				return nil, "", "", updateErr
			}
		}
		return nil, "", "", fmt.Errorf("review round has %d units, above normal fan-out %d; explicit authorization is required", len(units), round.MaxUnits)
	}
	unit, err := nextReviewUnit(ctx, rounds, run, *round, invocation.Role, requested, nil)
	if err != nil {
		return nil, "", "", err
	}
	if unit == nil {
		return nil, "", "", fmt.Errorf("no review unit is available for %s", invocation.Role)
	}
	if err := validateReviewArtifact(round.DiffPath, round.DiffBytes, round.DiffSHA256); err != nil {
		return nil, "", "", fmt.Errorf("validate canonical review diff: %w", err)
	}
	canonical, err := os.ReadFile(round.DiffPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("read canonical review diff: %w", err)
	}
	unitValue := reviewUnitForPartition(*unit)
	evidence, err := reviewunits.UnitDiff(canonical, unitValue)
	if err != nil {
		return nil, "", "", fmt.Errorf("derive review unit %q: %w", unit.UnitID, err)
	}
	if err := writeReviewUnitArtifact(invocation.InvocationDirectory, evidence); err != nil {
		return nil, "", "", err
	}
	if err := rounds.UpdateReviewRoundStatus(ctx, round.ID, store.ReviewRoundStatusActive); err != nil {
		return nil, "", "", err
	}
	base.CheckpointSHA = run.CheckpointSHA
	base.DiffPath = prompt.WorkerReviewDiffPath
	base.DiffBytes = round.DiffBytes
	base.DiffSHA256 = round.DiffSHA256
	base.CurrentDiff = ""
	base.OmittedDiffBytes = 0
	base.ChangedPathsCommand = ""
	base.DiffPathCommand = ""
	return reviewContextWithUnit(base, *round, *unit, len(units)), round.ID, unit.UnitID, nil
}

// runReviewUnitConfig extracts repository review policy from the frozen run
// packet, falling back to defaults for historical packets.
func runReviewUnitConfig(run store.Run) config.ReviewUnitConfig {
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return config.ReviewUnitConfig{}
	}
	return packet.RepositoryConfig.ReviewUnits
}

// validatePersistedReviewManifest verifies a frozen review round without
// changing its partition. Recovery rebuilds the expected manifest only as an
// in-memory identity check, then compares every normalized unit and artifact
// digest before a reviewer may be resumed.
func validatePersistedReviewManifest(ctx context.Context, rounds store.ReviewRoundStore, run store.Run, round store.ReviewRound) ([]store.ReviewUnit, error) {
	if round.RunID != run.ID {
		return nil, fmt.Errorf("review round %q belongs to run %q, want %q", round.ID, round.RunID, run.ID)
	}
	if round.CheckpointSHA != run.CheckpointSHA || round.BaseCheckpointSHA != reviewDiffBase(run) {
		return nil, fmt.Errorf("review round %q is not bound to the exact checkpoint and base", round.ID)
	}
	if round.ID != reviewRoundID(run, round.DiffSHA256) {
		return nil, fmt.Errorf("review round %q has an unexpected deterministic identity", round.ID)
	}
	if round.SchemaVersion != reviewunits.SchemaVersion || round.PolicyVersion != reviewunits.PolicyVersion {
		return nil, fmt.Errorf("review round %q uses unsupported partition policy %q/%d", round.ID, round.PolicyVersion, round.SchemaVersion)
	}
	if round.MaxUnitBytes <= 0 || round.MaxUnitBytes > config.MaxReviewUnitBytes || round.MaxUnits <= 0 || round.MaxUnits > store.MaxAuthorizedReviewUnits || round.ContextLines != reviewDiffContextLines || round.Concurrency <= 0 || round.Concurrency > 16 || round.AuthorizedMaxUnits < 0 || round.AuthorizedMaxUnits > store.MaxAuthorizedReviewUnits || (round.AuthorizedMaxUnits > 0 && round.AuthorizedMaxUnits < round.MaxUnits) {
		return nil, fmt.Errorf("review round %q has unsupported partition bounds", round.ID)
	}
	if err := validateReviewArtifact(round.DiffPath, round.DiffBytes, round.DiffSHA256); err != nil {
		return nil, fmt.Errorf("validate canonical review diff for round %q: %w", round.ID, err)
	}
	canonical, err := os.ReadFile(round.DiffPath)
	if err != nil {
		return nil, fmt.Errorf("read canonical review diff for round %q: %w", round.ID, err)
	}
	manifest, err := reviewunits.Build(canonical, run.ID, round.BaseCheckpointSHA, round.CheckpointSHA, reviewunits.Policy{
		SchemaVersion: reviewunits.SchemaVersion,
		PolicyVersion: reviewunits.PolicyVersion,
		MaxUnitBytes:  round.MaxUnitBytes,
		MaxUnits:      round.MaxUnits,
		ContextLines:  reviewDiffContextLines,
	})
	if err != nil {
		return nil, fmt.Errorf("revalidate review manifest %q: %w", round.ID, err)
	}
	if int64(manifest.DiffBytes) != round.DiffBytes || manifest.DiffSHA256 != round.DiffSHA256 || manifest.ManifestSHA256 != round.ManifestSHA256 || manifest.MaxUnitBytes != round.MaxUnitBytes || manifest.MaxUnits != round.MaxUnits || manifest.ContextLines != round.ContextLines {
		return nil, fmt.Errorf("review round %q manifest identity does not match its persisted policy or diff", round.ID)
	}
	if err := reviewunits.VerifyPrimaryCoverage(manifest); err != nil {
		return nil, fmt.Errorf("verify primary coverage for round %q: %w", round.ID, err)
	}
	units, err := rounds.ReviewUnits(ctx, round.ID)
	if err != nil {
		return nil, fmt.Errorf("read persisted units for round %q: %w", round.ID, err)
	}
	expected := reviewUnitsFromManifest(round.ID, manifest)
	if !sameReviewUnitManifest(expected, units) {
		return nil, fmt.Errorf("persisted units for round %q do not match its immutable manifest", round.ID)
	}
	for _, unit := range units {
		if _, err := reviewunits.UnitDiff(canonical, reviewUnitForPartition(unit)); err != nil {
			return nil, fmt.Errorf("validate persisted review unit %q: %w", unit.UnitID, err)
		}
	}
	return units, nil
}

// sameReviewUnitManifest compares normalized units without relying on the
// partitioner's public JSON shape. Store rows and partition units have
// different field tags, but their normalized store representation is exact.
func sameReviewUnitManifest(left, right []store.ReviewUnit) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// reviewUnitsFromManifest converts the deterministic manifest into normalized
// store rows while preserving empty-versus-absent JSON slice semantics.
func reviewUnitsFromManifest(roundID string, manifest reviewunits.Manifest) []store.ReviewUnit {
	units := make([]store.ReviewUnit, 0, len(manifest.Units))
	for _, unit := range manifest.Units {
		converted := store.ReviewUnit{RoundID: roundID, UnitID: unit.ID, Ordinal: unit.Ordinal, WorkloadBytes: unit.WorkloadBytes, DiffSHA256: unit.DiffSHA256, ChangedLines: unit.ChangedLines, PrimaryNonTextFiles: append([]string(nil), unit.PrimaryNonTextFiles...)}
		converted.Segments = make([]store.ReviewUnitSegment, 0, len(unit.Segments))
		for _, segment := range unit.Segments {
			converted.Segments = append(converted.Segments, store.ReviewUnitSegment{StartByte: segment.StartByte, EndByte: segment.EndByte})
		}
		for _, value := range unit.PrimaryRanges {
			converted.PrimaryRanges = append(converted.PrimaryRanges, store.ReviewUnitRange{Path: value.Path, Hunk: value.Hunk, Side: value.Side, StartLine: value.StartLine, EndLine: value.EndLine})
		}
		for _, value := range unit.ContextRanges {
			converted.ContextRanges = append(converted.ContextRanges, store.ReviewUnitRange{Path: value.Path, Hunk: value.Hunk, Side: value.Side, StartLine: value.StartLine, EndLine: value.EndLine})
		}
		units = append(units, converted)
	}
	return units
}

// reviewUnitForPartition converts a normalized store row to the partitioner
// representation used to derive and verify its artifact.
func reviewUnitForPartition(unit store.ReviewUnit) reviewunits.Unit {
	converted := reviewunits.Unit{ID: unit.UnitID, Ordinal: unit.Ordinal, WorkloadBytes: unit.WorkloadBytes, DiffSHA256: unit.DiffSHA256, ChangedLines: unit.ChangedLines, PrimaryNonTextFiles: append([]string(nil), unit.PrimaryNonTextFiles...)}
	for _, segment := range unit.Segments {
		converted.Segments = append(converted.Segments, reviewunits.Segment{StartByte: segment.StartByte, EndByte: segment.EndByte})
	}
	for _, value := range unit.PrimaryRanges {
		converted.PrimaryRanges = append(converted.PrimaryRanges, reviewunits.Range{Path: value.Path, Hunk: value.Hunk, Side: value.Side, StartLine: value.StartLine, EndLine: value.EndLine})
	}
	for _, value := range unit.ContextRanges {
		converted.ContextRanges = append(converted.ContextRanges, reviewunits.Range{Path: value.Path, Hunk: value.Hunk, Side: value.Side, StartLine: value.StartLine, EndLine: value.EndLine})
	}
	return converted
}

// reviewArtifactIdentity is the bounded byte identity of one on-disk diff.
type reviewArtifactIdentity struct {
	bytes  int64
	sha256 string
}

// inspectReviewArtifact hashes a regular diff artifact without retaining its
// contents in the coordinator's persistent projections.
func inspectReviewArtifact(path string) (reviewArtifactIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return reviewArtifactIdentity{}, fmt.Errorf("stat review artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return reviewArtifactIdentity{}, errors.New("review artifact is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return reviewArtifactIdentity{}, fmt.Errorf("open review artifact: %w", err)
	}
	digest := sha256.New()
	count, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return reviewArtifactIdentity{}, fmt.Errorf("hash review artifact: %w", copyErr)
	}
	if closeErr != nil {
		return reviewArtifactIdentity{}, fmt.Errorf("close review artifact: %w", closeErr)
	}
	return reviewArtifactIdentity{bytes: count, sha256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// validateReviewArtifact checks one artifact against its immutable byte and
// SHA-256 identity.
func validateReviewArtifact(path string, expectedBytes int64, expectedSHA string) error {
	identity, err := inspectReviewArtifact(path)
	if err != nil {
		return err
	}
	if identity.bytes != expectedBytes || identity.sha256 != expectedSHA {
		return fmt.Errorf("review artifact identity is %d/%s, want %d/%s", identity.bytes, identity.sha256, expectedBytes, expectedSHA)
	}
	return nil
}

// copyReviewArtifact atomically publishes the immutable canonical review diff.
func copyReviewArtifact(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create review round directory: %w", err)
	}
	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source review artifact: %w", err)
	}
	defer func() { _ = sourceFile.Close() }()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".review-round-*.tmp")
	if err != nil {
		return fmt.Errorf("create review round artifact temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure review round artifact temporary: %w", err)
	}
	if _, err := io.Copy(temporary, sourceFile); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy canonical review artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync canonical review artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close canonical review artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish canonical review artifact: %w", err)
	}
	remove = false
	return nil
}

// writeReviewUnitArtifact atomically publishes one invocation's derived unit
// evidence into its private packet directory.
func writeReviewUnitArtifact(directory string, evidence []byte) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create review unit directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".review-unit-*.tmp")
	if err != nil {
		return fmt.Errorf("create review unit temporary: %w", err)
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure review unit temporary: %w", err)
	}
	if _, err := temporary.Write(evidence); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write review unit artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync review unit artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close review unit artifact: %w", err)
	}
	destination := filepath.Join(directory, reviewUnitArtifactName)
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("publish review unit artifact: %w", err)
	}
	remove = false
	return nil
}

// reviewUnitResultFromReport converts one validated report into the durable
// axis-specific unit projection carried by a result-acceptance effect.
func reviewUnitResultFromReport(invocation store.Invocation, value report.Report, checkpoint string) store.ReviewUnitResult {
	result := store.ReviewUnitResult{RoundID: invocation.ReviewRoundID, Role: invocation.Role, UnitID: invocation.ReviewUnitID, InvocationID: invocation.ID, CheckpointSHA: checkpoint, Outcome: string(value.Outcome), Summary: value.Summary}
	if value.ReviewHandoff != nil {
		for _, finding := range value.ReviewHandoff.Findings {
			result.Findings = append(result.Findings, store.ReviewFinding{Location: finding.Location, Claim: finding.Claim, Evidence: finding.Evidence, Severity: string(finding.Severity), Category: string(finding.Category), SuggestedResolution: finding.SuggestedResolution, SuggestedOwner: finding.SuggestedOwner, UnitID: invocation.ReviewUnitID})
		}
	}
	for _, question := range value.Questions {
		result.Questions = append(result.Questions, store.ReviewQuestion{ID: question.ID, Prompt: question.Prompt})
	}
	for _, evidence := range value.Evidence {
		result.Evidence = append(result.Evidence, store.ReviewEvidence{Kind: evidence.Kind, Detail: evidence.Detail})
	}
	return result
}

// aggregateReviewUnitResults reduces a complete ordered axis into the
// existing role-level review projection. Until every manifest unit has a
// result it returns complete=false and deliberately exposes no aggregate.
func aggregateReviewUnitResults(ctx context.Context, rounds store.ReviewRoundStore, round store.ReviewRound, role, checkpoint string) (*store.ReviewResult, bool, error) {
	units, err := rounds.ReviewUnits(ctx, round.ID)
	if err != nil {
		return nil, false, fmt.Errorf("read review units for aggregate: %w", err)
	}
	results, err := rounds.ReviewUnitResults(ctx, round.ID, role)
	if err != nil {
		return nil, false, fmt.Errorf("read %s review unit results for aggregate: %w", role, err)
	}
	byID := make(map[string]store.ReviewUnitResult, len(results))
	for _, result := range results {
		if result.Role != role || result.RoundID != round.ID || result.CheckpointSHA != checkpoint {
			return nil, false, fmt.Errorf("%s review unit result %s has an inconsistent identity", role, result.UnitID)
		}
		if _, exists := byID[result.UnitID]; exists {
			return nil, false, fmt.Errorf("%s review unit %s has duplicate results", role, result.UnitID)
		}
		byID[result.UnitID] = result
	}
	if len(byID) > len(units) {
		return nil, false, fmt.Errorf("%s review has %d unit results for %d manifest units", role, len(byID), len(units))
	}
	for _, unit := range units {
		if _, exists := byID[unit.UnitID]; !exists {
			return nil, false, nil
		}
	}
	if len(units) == 0 {
		return nil, false, errors.New("review round has no units to aggregate")
	}

	result := &store.ReviewResult{CheckpointSHA: checkpoint, Findings: make([]store.ReviewFinding, 0)}
	var summaries []string
	var clarificationQuestions []store.ReviewQuestion
	var cannotProceedEvidence []store.ReviewEvidence
	questionIDs := make(map[string]struct{})
	hasClarification, hasCannotProceed := false, false
	for _, unit := range units {
		unitResult := byID[unit.UnitID]
		if strings.TrimSpace(unitResult.Summary) != "" {
			summaries = append(summaries, fmt.Sprintf("unit %s: %s", unit.UnitID, unitResult.Summary))
		}
		switch unitResult.Outcome {
		case store.ReviewUnitOutcomeCannotProceed:
			hasCannotProceed = true
			cannotProceedEvidence = append(cannotProceedEvidence, unitResult.Evidence...)
		case store.ReviewUnitOutcomeNeedsClarification:
			hasClarification = true
			for _, question := range unitResult.Questions {
				if _, exists := questionIDs[question.ID]; exists {
					question.ID = unit.UnitID + "." + question.ID
				}
				if _, exists := questionIDs[question.ID]; exists {
					continue
				}
				questionIDs[question.ID] = struct{}{}
				clarificationQuestions = append(clarificationQuestions, question)
			}
		}
		for _, finding := range unitResult.Findings {
			if finding.UnitID == "" {
				finding.UnitID = unit.UnitID
			}
			result.Findings = append(result.Findings, finding)
		}
	}
	if len(result.Findings) > 64 {
		return nil, false, fmt.Errorf("%s review findings exceed the 64-entry axis limit", role)
	}
	if hasCannotProceed {
		result.Outcome = store.ReviewUnitOutcomeCannotProceed
		result.Evidence = cannotProceedEvidence
	} else if hasClarification {
		result.Outcome = store.ReviewUnitOutcomeNeedsClarification
		result.Questions = clarificationQuestions
	} else {
		result.Outcome = store.ReviewUnitOutcomeCompleted
	}
	if len(result.Questions) > 32 || len(result.Evidence) > 32 {
		return nil, false, fmt.Errorf("%s review disposition exceeds its 32-entry aggregate limit", role)
	}
	result.Summary = strings.Join(summaries, "; ")
	if len(result.Summary) > 4000 {
		result.Summary = result.Summary[:4000]
	}
	return result, true, nil
}

// recoverPartitionedReviewAggregates repairs the coordinator-facing role
// projections after a crash that cleared result acceptance before its axis
// aggregate was dispatched. Normalized unit results are the source of truth;
// this path never reads a report or invents a new invocation.
func (s *Service) recoverPartitionedReviewAggregates(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run) error {
	if run == nil || run.Stage != store.StageReview || store.IsTerminalStatus(run.Status) || run.Status == store.StatusWaitingForHarness {
		return nil
	}
	partitioned, ok := runStore.(store.ReviewRoundStore)
	if !ok {
		return nil
	}
	round, err := partitioned.ReviewRound(ctx, run.ID, run.CheckpointSHA)
	if err != nil {
		return fmt.Errorf("read review round for aggregate recovery: %w", err)
	}
	if round == nil {
		return nil
	}
	units, err := validatePersistedReviewManifest(ctx, partitioned, *run, *round)
	if err != nil {
		return err
	}
	if round.Status == store.ReviewRoundStatusAwaitingAuthorization || round.Status == store.ReviewRoundStatusError {
		return nil
	}
	active, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return fmt.Errorf("read active invocations for review aggregate recovery: %w", err)
	}
	if supported {
		for _, invocation := range active {
			if currentReviewInvocation(invocation) {
				return nil
			}
		}
	}
	invocationStore, hasInvocations := runStore.(InvocationStore)
	if !hasInvocations {
		return nil
	}
	for _, role := range reviewAxisRoles {
		if !reviewRoleConfigured(*run, role) || checkpointReviewForRole(*run, role) != nil {
			continue
		}
		results, err := partitioned.ReviewUnitResults(ctx, round.ID, role)
		if err != nil {
			return fmt.Errorf("read %s results for aggregate recovery: %w", role, err)
		}
		if len(results) != len(units) {
			continue
		}
		aggregate, complete, err := aggregateReviewUnitResults(ctx, partitioned, *round, role, run.CheckpointSHA)
		if err != nil {
			return err
		}
		if !complete || aggregate == nil {
			continue
		}
		last := results[len(results)-1]
		invocation, err := invocationStore.Invocation(ctx, run.ID, last.InvocationID)
		if err != nil {
			return fmt.Errorf("read %s aggregate recovery invocation %q: %w", role, last.InvocationID, err)
		}
		if invocation == nil {
			return fmt.Errorf("%s aggregate recovery invocation %q is missing", role, last.InvocationID)
		}
		if invocation.RunID != run.ID || invocation.Role != role || invocation.ReviewRoundID != round.ID || invocation.ReviewUnitID != last.UnitID || invocation.Status == store.InvocationStatusActive {
			return fmt.Errorf("%s aggregate recovery invocation %q has inconsistent review identity", role, invocation.ID)
		}
		value := reviewReportFromAggregate(*aggregate)
		if _, err := s.acceptReviewReport(ctx, registration, runStore, run, invocation, value); err != nil {
			return fmt.Errorf("project %s aggregate after restart: %w", role, err)
		}
	}
	return nil
}

// reviewReportFromAggregate reconstructs the report-shaped payload used by
// the existing acceptance seam while retaining every content-bearing field
// already persisted in normalized unit results.
func reviewReportFromAggregate(value store.ReviewResult) report.Report {
	result := report.Report{Outcome: report.Outcome(value.Outcome), Summary: value.Summary}
	if value.Outcome == string(store.ReviewUnitOutcomeCompleted) || len(value.Findings) > 0 {
		result.ReviewHandoff = &report.ReviewHandoff{ReviewedSHA: value.CheckpointSHA, Findings: make([]report.ReviewFinding, 0, len(value.Findings))}
		for _, finding := range value.Findings {
			result.ReviewHandoff.Findings = append(result.ReviewHandoff.Findings, report.ReviewFinding{
				Location:            finding.Location,
				Claim:               finding.Claim,
				Evidence:            finding.Evidence,
				Severity:            report.ReviewSeverity(finding.Severity),
				Category:            report.ReviewCategory(finding.Category),
				SuggestedResolution: finding.SuggestedResolution,
				SuggestedOwner:      finding.SuggestedOwner,
				UnitID:              finding.UnitID,
			})
		}
	}
	for _, question := range value.Questions {
		result.Questions = append(result.Questions, report.Question{ID: question.ID, Prompt: question.Prompt})
	}
	for _, evidence := range value.Evidence {
		result.Evidence = append(result.Evidence, report.Evidence{Kind: evidence.Kind, Detail: evidence.Detail})
	}
	return result
}

// reviewManifestTerminal reports whether every configured review axis has one
// terminal result for every persisted unit. Unit rows are the source of truth;
// role-level projections are deliberately not used to infer completion.
func reviewManifestTerminal(ctx context.Context, rounds store.ReviewRoundStore, round store.ReviewRound, run store.Run) (bool, error) {
	units, err := rounds.ReviewUnits(ctx, round.ID)
	if err != nil {
		return false, fmt.Errorf("read review units for terminal status: %w", err)
	}
	if len(units) == 0 {
		return false, errors.New("review round has no units for terminal status")
	}
	expected := make(map[string]struct{}, len(units))
	for _, unit := range units {
		expected[unit.UnitID] = struct{}{}
	}
	for _, role := range reviewAxisRoles {
		if !reviewRoleConfigured(run, role) {
			continue
		}
		results, resultErr := rounds.ReviewUnitResults(ctx, round.ID, role)
		if resultErr != nil {
			return false, fmt.Errorf("read %s results for terminal status: %w", role, resultErr)
		}
		if len(results) != len(units) {
			return false, nil
		}
		seen := make(map[string]struct{}, len(results))
		for _, result := range results {
			if result.RoundID != round.ID || result.Role != role || result.CheckpointSHA != run.CheckpointSHA {
				return false, fmt.Errorf("%s review unit result %q has inconsistent terminal identity", role, result.UnitID)
			}
			if _, exists := expected[result.UnitID]; !exists {
				return false, fmt.Errorf("%s review unit result %q is outside the manifest", role, result.UnitID)
			}
			if _, exists := seen[result.UnitID]; exists {
				return false, fmt.Errorf("%s review unit result %q is duplicated", role, result.UnitID)
			}
			seen[result.UnitID] = struct{}{}
		}
	}
	return true, nil
}

// reviewResumeAgentRequest selects the next persisted review unit for manual
// recovery. It keeps a restart from re-entering an already aggregated axis and
// preserves the manifest order shared by both reviewers.
func reviewResumeAgentRequest(ctx context.Context, runStore RunStore, run store.Run) (AgentRequest, error) {
	request := agentRequestForRun(run)
	if run.Stage != store.StageReview {
		return request, nil
	}
	partitioned, ok := runStore.(store.ReviewRoundStore)
	if !ok {
		return request, nil
	}
	round, err := partitioned.ReviewRound(ctx, run.ID, run.CheckpointSHA)
	if err != nil {
		return AgentRequest{}, fmt.Errorf("read review round for resume: %w", err)
	}
	if round == nil {
		return request, nil
	}
	units, err := validatePersistedReviewManifest(ctx, partitioned, run, *round)
	if err != nil {
		return AgentRequest{}, err
	}
	if round.Status == store.ReviewRoundStatusAwaitingAuthorization || round.Status == store.ReviewRoundStatusError || round.Status == store.ReviewRoundStatusComplete {
		return AgentRequest{}, fmt.Errorf("review round %q is not resumable with status %q", round.ID, round.Status)
	}
	for _, role := range reviewAxisRoles {
		if !reviewRoleConfigured(run, role) {
			continue
		}
		results, resultErr := partitioned.ReviewUnitResults(ctx, round.ID, role)
		if resultErr != nil {
			return AgentRequest{}, fmt.Errorf("read %s review results for resume: %w", role, resultErr)
		}
		completed := make(map[string]struct{}, len(results))
		for _, result := range results {
			completed[result.UnitID] = struct{}{}
		}
		definition, declared := workflow.DefaultRegistry().Role(role)
		if !declared {
			return AgentRequest{}, fmt.Errorf("workflow role %q is not declared", role)
		}
		for _, unit := range units {
			if _, exists := completed[unit.UnitID]; !exists {
				return AgentRequest{RunID: run.ID, Role: role, Stage: definition.Stage, ReviewUnitID: unit.UnitID}, nil
			}
		}
	}
	return AgentRequest{}, fmt.Errorf("review round %q has no missing unit to resume", round.ID)
}
