// Package report defines the structured handoff exchanged between a harness
// and the coordinator. It deliberately contains summaries and evidence, not
// transcripts or private chain-of-thought.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// SchemaVersion is the version of the agent report envelope.
const SchemaVersion = 1

// MaxReportBytes is the maximum serialized report size accepted by the
// coordinator and report writer.
const MaxReportBytes = 256 << 10

const (
	maxSummaryRunes       = 2000
	maxChangeSummaryRunes = 4000
	maxTextRunes          = 4000
	maxPathRunes          = 1024
	maxIdentifierRunes    = 256
	maxAcceptanceMappings = 64
	maxProductionFiles    = 256
	maxFocusedCommands    = 128
	maxKnownLimitations   = 64
	maxPermittedPaths     = 128
	maxQuestions          = 32
	maxEvidenceEntries    = 32
	maxEvaluationSignals  = 32
	// maxTestFiles bounds changed test and test-infrastructure paths in one handoff.
	maxTestFiles = 256
	// maxUncoveredCriteria bounds criteria the test role explicitly leaves open.
	maxUncoveredCriteria = 64
	maxReviewFindings    = 128
)

// ReportFileName is the only accepted report filename in an invocation result
// directory.
const ReportFileName = "report.json"

// Outcome is the complete set of decisions a harness may propose.
type Outcome string

const (
	// OutcomeCompleted proposes a structured implementation handoff.
	OutcomeCompleted Outcome = "completed"
	// OutcomeNeedsClarification proposes questions for an authorized human.
	OutcomeNeedsClarification Outcome = "needs_clarification"
	// OutcomeCannotProceed proposes an evidence-backed inability to continue.
	OutcomeCannotProceed Outcome = "cannot_proceed"
)

// AcceptanceMapping connects one ticket criterion to evidence in the handoff.
type AcceptanceMapping struct {
	// Criterion is the criterion or requirement being addressed.
	Criterion string `json:"criterion"`
	// Evidence identifies a command, file, or other observable evidence.
	Evidence string `json:"evidence"`
}

// Handoff contains the implementation information accepted after completion.
type Handoff struct {
	// ChangeSummary describes the implementation in ordinary engineering terms.
	ChangeSummary string `json:"change_summary"`
	// AcceptanceMapping maps requirements to observable evidence.
	AcceptanceMapping []AcceptanceMapping `json:"acceptance_mapping"`
	// ProductionFilesChanged lists repository-relative files changed by the role.
	ProductionFilesChanged []string `json:"production_files_changed"`
	// FocusedCommands lists focused commands actually run by the role.
	FocusedCommands []string `json:"focused_commands"`
	// KnownLimitations records limitations that remain visible to later stages.
	KnownLimitations []string `json:"known_limitations,omitempty"`
}

// ReviewSeverity classifies the urgency of one specification-review finding.
type ReviewSeverity string

const (
	// ReviewSeverityBlocker identifies a concrete violation that prevents readiness.
	ReviewSeverityBlocker ReviewSeverity = "blocker"
	// ReviewSeverityAdvisory identifies a visible, non-gating observation.
	ReviewSeverityAdvisory ReviewSeverity = "advisory"
)

// ReviewCategory identifies the bounded class of a review observation.
type ReviewCategory string

const (
	ReviewCategoryCorrectness   ReviewCategory = "correctness"
	ReviewCategorySecurity      ReviewCategory = "security"
	ReviewCategorySpecification ReviewCategory = "specification"
	ReviewCategoryStandards     ReviewCategory = "standards"
	ReviewCategoryDocumentedStandards ReviewCategory = "documented_standards"
	ReviewCategoryTaste         ReviewCategory = "taste"
	ReviewCategoryScope         ReviewCategory = "scope"
)

// ReviewFinding is a complete, content-limited observation from the independent reviewer.
type ReviewFinding struct {
	Location            string         `json:"location"`
	Claim               string         `json:"claim"`
	Evidence            string         `json:"evidence"`
	Severity            ReviewSeverity `json:"severity"`
	Category            ReviewCategory `json:"category"`
	SuggestedResolution string         `json:"suggested_resolution"`
	SuggestedOwner      string         `json:"suggested_owner"`
}

// ReviewHandoff binds all findings to the immutable checkpoint they inspected.
type ReviewHandoff struct {
	ReviewedSHA string          `json:"reviewed_sha"`
	Findings    []ReviewFinding `json:"findings"`
}

// TestHandoff contains the bounded evidence needed to transfer a test-stage
// result to implementation. It deliberately excludes production files and
// requires the coordinator to verify the focused command independently.
type TestHandoff struct {
	// AcceptanceCoverage maps frozen acceptance criteria to test evidence.
	AcceptanceCoverage []AcceptanceMapping `json:"acceptance_coverage"`
	// ChangedFiles lists repository-relative test or test-infrastructure paths.
	ChangedFiles []string `json:"changed_files"`
	// FocusedTestCommand is the exact command the coordinator reruns for red
	// verification.
	FocusedTestCommand string `json:"focused_test_command"`
	// ExpectedFailureReason is the bounded failure identifier that must appear
	// in the coordinator-captured rerun output.
	ExpectedFailureReason string `json:"expected_failure_reason"`
	// ObservedFailureEvidence is content-limited evidence supplied by the role.
	ObservedFailureEvidence []Evidence `json:"observed_failure_evidence"`
	// InfrastructureChanges lists the subset of changed files that support tests.
	InfrastructureChanges []string `json:"infrastructure_changes,omitempty"`
	// UncoveredCriteria records acceptance criteria not covered by this handoff.
	UncoveredCriteria []string `json:"uncovered_criteria,omitempty"`
}

// Question identifies one clarification requested from an authorized human.
type Question struct {
	// ID is stable within the specification packet and report.
	ID string `json:"id"`
	// Prompt is the plain-language question to post on the active work item.
	Prompt string `json:"prompt"`
}

// Evidence is a content-limited observation supporting an inability to proceed.
type Evidence struct {
	// Kind classifies the evidence, such as command, file, or environment.
	Kind string `json:"kind"`
	// Detail identifies the observable evidence without embedding a transcript.
	Detail string `json:"detail"`
}

// EscalationCategory identifies a content-free workflow escalation that a
// harness may report for later human disposition.
type EscalationCategory string

const (
	// EscalationTestDispute identifies a disputed deterministic test result.
	EscalationTestDispute EscalationCategory = "test_dispute"
	// EscalationReviewFinding identifies an escalated review finding.
	EscalationReviewFinding EscalationCategory = "review_finding"
)

// Exemption identifies a content-free policy exemption reported by a harness.
type Exemption string

const (
	// ExemptionHuman identifies a human-approved exemption.
	ExemptionHuman Exemption = "human"
	// ExemptionTechnical identifies an infrastructure exemption.
	ExemptionTechnical Exemption = "technical"
	// ExemptionBaseline identifies an accepted pre-existing baseline condition.
	ExemptionBaseline Exemption = "baseline"
)

// BlockerCategory identifies a repeated content-free blocker class.
type BlockerCategory string

const (
	// BlockerSetup identifies dependency setup blockage.
	BlockerSetup BlockerCategory = "setup"
	// BlockerGate identifies deterministic gate blockage.
	BlockerGate BlockerCategory = "gate"
	// BlockerTest identifies test-policy blockage.
	BlockerTest BlockerCategory = "test"
	// BlockerReview identifies review blockage.
	BlockerReview BlockerCategory = "review"
	// BlockerHarness identifies harness blockage.
	BlockerHarness BlockerCategory = "harness"
	// BlockerBudget identifies budget blockage.
	BlockerBudget BlockerCategory = "budget"
	// BlockerUnknown identifies an unavailable stable blocker class.
	BlockerUnknown BlockerCategory = "unknown"
)

// Usage contains only reliable harness-reported measurements. It is optional;
// an omitted value means the coordinator must retain usage as unavailable.
type Usage struct {
	// Available distinguishes reported measurements from an unavailable value.
	Available bool `json:"available"`
	// InputTokens is the reported input-token count, when supplied.
	InputTokens int64 `json:"input_tokens,omitempty"`
	// OutputTokens is the reported output-token count, when supplied.
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// TotalTokens is the reported total-token count, when supplied.
	TotalTokens int64 `json:"total_tokens,omitempty"`
	// CostReported distinguishes a reported zero cost from no cost measurement.
	CostReported bool `json:"cost_reported,omitempty"`
	// CostMicros is the exact harness-reported cost in millionths of Currency.
	CostMicros int64 `json:"cost_micros,omitempty"`
	// Currency is the three-letter uppercase currency for CostMicros.
	Currency string `json:"currency,omitempty"`
}

// Report is the schema-versioned proposal written by one invocation.
type Report struct {
	// SchemaVersion identifies this report envelope shape.
	SchemaVersion int `json:"schema_version"`
	// InvocationID binds the report to one immutable invocation.
	InvocationID string `json:"invocation_id"`
	// RunID binds the report to its factory run.
	RunID string `json:"run_id"`
	// Harness identifies the configured harness, for example codex.
	Harness string `json:"harness"`
	// Role identifies the workflow role that owns the invocation.
	Role string `json:"role"`
	// Stage identifies the workflow stage that owns the invocation.
	Stage string `json:"stage"`
	// Outcome is exactly one coordinator-recognized proposal.
	Outcome Outcome `json:"outcome"`
	// Summary is a short observable description of the proposed result.
	Summary string `json:"summary"`
	// Handoff is required only for a completed outcome.
	Handoff *Handoff `json:"handoff,omitempty"`
	// ReviewHandoff is required only for a completed specification-review report.
	ReviewHandoff *ReviewHandoff `json:"review_handoff,omitempty"`
	// TestHandoff is required for a completed test-stage outcome unless a
	// technical exemption is explicitly reported.
	TestHandoff *TestHandoff `json:"test_handoff,omitempty"`
	// Questions are required only for a clarification outcome.
	Questions []Question `json:"questions,omitempty"`
	// Evidence is required only for a cannot-proceed outcome.
	Evidence []Evidence `json:"evidence,omitempty"`
	// BudgetExhausted is an explicit harness signal, never inferred by the host.
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`
	// Exemptions contains only fixed policy-exemption categories.
	Exemptions []Exemption `json:"exemptions,omitempty"`
	// Escalations contains only fixed human-disposition categories.
	Escalations []EscalationCategory `json:"escalations,omitempty"`
	// Blockers contains repeated fixed blocker categories without blocker text.
	Blockers []BlockerCategory `json:"blockers,omitempty"`
	// Usage is optional reliable usage or cost reported by the harness.
	Usage *Usage `json:"usage,omitempty"`
	// NativeSessionID is the harness-native continuation identifier, when known.
	NativeSessionID string `json:"native_session_id,omitempty"`
	// ReportedAt records when the harness published the proposal.
	ReportedAt time.Time `json:"reported_at"`
}

// ValidationContext binds a report to the coordinator's immutable invocation
// and observed worktree state.
type ValidationContext struct {
	// InvocationID is the expected immutable invocation identifier.
	InvocationID string
	// RunID is the expected factory run identifier.
	RunID string
	// Harness is the expected configured harness.
	Harness string
	// Role is the expected workflow role.
	Role string
	// Stage is the expected workflow stage.
	Stage string
	// CheckpointSHA is the exact commit a review report must have inspected.
	CheckpointSHA string
	// WorktreePath is the host checkout root, when filesystem containment is checked.
	WorktreePath string
	// PermittedPaths contains repository-relative prefixes the role may report.
	PermittedPaths []string
	// ObservedChanges contains coordinator-observed repository-relative changes.
	ObservedChanges []string
	// WorktreeObserved distinguishes an inspected clean worktree from a
	// coordinator that has no worktree-inspection capability.
	WorktreeObserved bool
	// TestPaths contains configured repository-relative test prefixes.
	TestPaths []string
	// TestInfrastructurePaths contains configured repository-relative prefixes
	// for essential test infrastructure.
	TestInfrastructurePaths []string
	// deferTestPathPolicy defers repository-specific ownership checks until the
	// coordinator supplies the frozen configuration during report acceptance.
	deferTestPathPolicy bool
}

// WriteAtomic validates and publishes a report as report.json below the
// invocation-specific result directory. A temporary file is synced and
// renamed so readers never observe a partial JSON document.
func WriteAtomic(resultDirectory string, value Report) (string, error) {
	if err := Validate(value, identityFrom(value, value.InvocationID)); err != nil {
		return "", err
	}
	if err := validateInvocationDirectory(resultDirectory, value.InvocationID); err != nil {
		return "", err
	}
	return writeAtomicReport(resultDirectory, value)
}

// WriteAtomicForInvocation publishes a report into a worker-mounted result
// directory whose host path is already scoped to the invocation. The explicit
// identity parameter keeps the protocol safe even though the in-worker mount
// itself is named /results rather than after the host invocation id.
func WriteAtomicForInvocation(resultDirectory, invocationID string, value Report) (string, error) {
	if err := Validate(value, identityFrom(value, invocationID)); err != nil {
		return "", err
	}
	if !filepath.IsAbs(resultDirectory) {
		return "", errors.New("invocation result directory must be absolute")
	}
	if !safeIdentifier(invocationID) {
		return "", errors.New("invocation id contains unsafe characters")
	}
	return writeAtomicReport(resultDirectory, value)
}

// writeAtomicReport performs the filesystem publication shared by the two
// public report-writing entry points.
func writeAtomicReport(resultDirectory string, value Report) (string, error) {
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create invocation result directory: %w", err)
	}
	if info, err := os.Lstat(resultDirectory); err != nil {
		return "", fmt.Errorf("inspect invocation result directory: %w", err)
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("invocation result directory must be a real directory")
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode agent report: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxReportBytes {
		return "", fmt.Errorf("agent report exceeds the %d-byte limit", MaxReportBytes)
	}
	temporary, err := os.CreateTemp(resultDirectory, ".report-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary agent report: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("protect temporary agent report: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary agent report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary agent report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary agent report: %w", err)
	}
	path := filepath.Join(resultDirectory, ReportFileName)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("agent report path must not be a symbolic link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect agent report path: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("publish agent report: %w", err)
	}
	removeTemporary = false
	return path, nil
}

// Read loads and validates the JSON envelope at path without accepting unknown
// fields or trailing data.
func Read(path string) (Report, error) {
	value, err := ReadEnvelope(path)
	if err != nil {
		return Report{}, err
	}
	if err := Validate(value, identityFrom(value, value.InvocationID)); err != nil {
		return Report{}, err
	}
	return value, nil
}

// ReadEnvelope loads a syntactically valid report envelope without applying
// semantic validation, allowing the coordinator to route unverifiable test
// evidence to human disposition instead of leaving an active run unattended.
func ReadEnvelope(path string) (Report, error) {
	cleanPath := filepath.Clean(path)
	directoryFD, err := unix.Open(filepath.Dir(cleanPath), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return Report{}, errors.New("agent report path must not be a symbolic link")
		}
		return Report{}, fmt.Errorf("inspect agent report: %w", err)
	}
	defer func() { _ = unix.Close(directoryFD) }()
	fileDescriptor, err := unix.Openat(directoryFD, filepath.Base(cleanPath), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return Report{}, errors.New("agent report path must not be a symbolic link")
		}
		return Report{}, fmt.Errorf("read agent report: %w", err)
	}
	file := os.NewFile(uintptr(fileDescriptor), cleanPath)
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return Report{}, errors.New("read agent report: open returned an invalid file descriptor")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Report{}, fmt.Errorf("read agent report: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Report{}, errors.New("agent report path must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(MaxReportBytes)+1))
	if err != nil {
		return Report{}, fmt.Errorf("read agent report: %w", err)
	}
	if len(data) > MaxReportBytes {
		return Report{}, fmt.Errorf("agent report exceeds the %d-byte limit", MaxReportBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value Report
	if err := decoder.Decode(&value); err != nil {
		return Report{}, fmt.Errorf("decode agent report: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Report{}, errors.New("agent report contains more than one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Report{}, fmt.Errorf("read trailing agent report data: %w", err)
	}
	return value, nil
}

// Validate checks identity, outcome shape, path safety, and observed worktree
// containment before a coordinator accepts a report.
func Validate(value Report, context ValidationContext) error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported report schema_version %d", value.SchemaVersion)
	}
	identityFields := []struct {
		field    string
		expected string
		actual   string
	}{
		{"invocation_id", context.InvocationID, value.InvocationID},
		{"run_id", context.RunID, value.RunID},
		{"harness", context.Harness, value.Harness},
		{"role", context.Role, value.Role},
		{"stage", context.Stage, value.Stage},
	}
	for _, identity := range identityFields {
		if strings.TrimSpace(identity.expected) == "" || identity.actual != identity.expected {
			return fmt.Errorf("report %s %q does not match expected %q", identity.field, identity.actual, identity.expected)
		}
	}
	if !safeIdentifier(value.InvocationID) || !safeIdentifier(value.RunID) {
		return errors.New("report identifiers contain unsafe characters")
	}
	if err := validateText("report summary", value.Summary, maxSummaryRunes, true); err != nil {
		return err
	}
	if value.ReportedAt.IsZero() {
		return errors.New("report reported_at is required")
	}
	switch value.Outcome {
	case OutcomeCompleted:
		if isReviewReport(value) {
			if value.Handoff != nil || value.TestHandoff != nil {
				return errors.New("specification review report must contain only a review handoff")
			}
			if value.ReviewHandoff == nil {
				return errors.New("completed specification review requires a review handoff")
			}
			if err := validateReviewHandoff(*value.ReviewHandoff, context); err != nil {
				return err
			}
		} else if isTestReport(value) {
			if value.Handoff != nil {
				return errors.New("test completed report must contain only a test handoff")
			}
			if value.TestHandoff == nil {
				if !hasExemption(value.Exemptions, ExemptionTechnical) {
					return errors.New("completed test report requires a test handoff")
				}
			} else if err := validateTestHandoff(*value.TestHandoff, context); err != nil {
				return err
			}
		} else {
			if value.ReviewHandoff != nil {
				return errors.New("implementation completed report must not contain a review handoff")
			}
			if value.Handoff == nil {
				return errors.New("completed report requires a handoff")
			}
			if value.TestHandoff != nil {
				return errors.New("implementation completed report must not contain a test handoff")
			}
			if err := validateHandoff(*value.Handoff, context); err != nil {
				return err
			}
		}
		if len(value.Questions) != 0 || len(value.Evidence) != 0 {
			return errors.New("completed report must contain only a handoff")
		}
	case OutcomeNeedsClarification:
		if len(value.Questions) == 0 {
			return errors.New("needs_clarification report requires at least one question")
		}
		if value.Handoff != nil || value.TestHandoff != nil || value.ReviewHandoff != nil || len(value.Evidence) != 0 {
			return errors.New("needs_clarification report must contain only questions")
		}
		if err := validateQuestions(value.Questions); err != nil {
			return err
		}
	case OutcomeCannotProceed:
		if len(value.Evidence) == 0 {
			return errors.New("cannot_proceed report requires evidence")
		}
		if value.Handoff != nil || value.TestHandoff != nil || value.ReviewHandoff != nil || len(value.Questions) != 0 {
			return errors.New("cannot_proceed report must contain only evidence")
		}
		if err := validateEvidence(value.Evidence); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported report outcome %q", value.Outcome)
	}
	if value.NativeSessionID != "" && !safeIdentifier(value.NativeSessionID) {
		return errors.New("native_session_id contains unsafe characters")
	}
	if value.Usage != nil {
		if err := validateUsage(*value.Usage); err != nil {
			return err
		}
	}
	if err := validateEvaluationSignals(value); err != nil {
		return err
	}
	return nil
}

// validateEvaluationSignals accepts only bounded categories and explicit
// harness-provided budget state, keeping work content outside the report's
// evaluation projection.
func validateEvaluationSignals(value Report) error {
	if len(value.Exemptions) > maxEvaluationSignals || len(value.Escalations) > maxEvaluationSignals || len(value.Blockers) > maxEvaluationSignals {
		return errors.New("report evaluation signal count exceeds the limit")
	}
	for _, exemption := range value.Exemptions {
		switch exemption {
		case ExemptionHuman, ExemptionTechnical, ExemptionBaseline:
		default:
			return fmt.Errorf("unsupported report exemption %q", exemption)
		}
	}
	for _, escalation := range value.Escalations {
		switch escalation {
		case EscalationTestDispute, EscalationReviewFinding:
		default:
			return fmt.Errorf("unsupported report escalation %q", escalation)
		}
	}
	for _, blocker := range value.Blockers {
		switch blocker {
		case BlockerSetup, BlockerGate, BlockerTest, BlockerReview, BlockerHarness, BlockerBudget, BlockerUnknown:
		default:
			return fmt.Errorf("unsupported report blocker %q", blocker)
		}
	}
	return nil
}

// validateUsage ensures a report never presents guessed or malformed harness
// measurements as reliable usage or cost.
func validateUsage(value Usage) error {
	if !value.Available {
		return errors.New("reported usage must be marked available")
	}
	if value.InputTokens < 0 || value.OutputTokens < 0 || value.TotalTokens < 0 || value.CostMicros < 0 {
		return errors.New("reported usage values must not be negative")
	}
	if value.InputTokens == 0 && value.OutputTokens == 0 && value.TotalTokens == 0 && !value.CostReported {
		return errors.New("reported usage must contain at least one measurement")
	}
	if value.CostReported {
		if !validCurrency(value.Currency) {
			return errors.New("reported cost requires a three-letter uppercase currency")
		}
	} else if value.CostMicros != 0 || value.Currency != "" {
		return errors.New("reported cost metadata requires cost_reported")
	}
	return nil
}

// validCurrency accepts the bounded ISO-style identifier used for reported
// cost metadata without accepting arbitrary content.
func validCurrency(value string) bool {
	if len(value) != 3 || strings.ToUpper(value) != value {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

// validateHandoff checks handoff text, path safety, permitted prefixes, and
// agreement with the coordinator's observed worktree changes.
func validateHandoff(value Handoff, context ValidationContext) error {
	if err := validateText("completed handoff change_summary", value.ChangeSummary, maxChangeSummaryRunes, true); err != nil {
		return err
	}
	if len(value.AcceptanceMapping) == 0 {
		return errors.New("completed handoff acceptance_mapping is required")
	}
	if len(value.AcceptanceMapping) > maxAcceptanceMappings {
		return fmt.Errorf("completed handoff acceptance_mapping exceeds %d entries", maxAcceptanceMappings)
	}
	for index, mapping := range value.AcceptanceMapping {
		if err := validateText(fmt.Sprintf("acceptance_mapping[%d].criterion", index), mapping.Criterion, maxTextRunes, true); err != nil {
			return err
		}
		if err := validateText(fmt.Sprintf("acceptance_mapping[%d].evidence", index), mapping.Evidence, maxTextRunes, true); err != nil {
			return err
		}
	}
	observed := make(map[string]struct{}, len(context.ObservedChanges))
	for _, path := range context.ObservedChanges {
		clean, err := cleanRelativePath(path)
		if err != nil {
			return fmt.Errorf("observed worktree path %q: %w", path, err)
		}
		observed[clean] = struct{}{}
		if len(context.PermittedPaths) > 0 && !withinAnyPrefix(clean, context.PermittedPaths) {
			return fmt.Errorf("observed worktree path %q is outside permitted paths", path)
		}
	}
	if len(value.ProductionFilesChanged) > maxProductionFiles {
		return fmt.Errorf("production_files_changed exceeds %d entries", maxProductionFiles)
	}
	for index, path := range value.ProductionFilesChanged {
		if err := validateText(fmt.Sprintf("production_files_changed[%d]", index), path, maxPathRunes, true); err != nil {
			return err
		}
		clean, err := cleanRelativePath(path)
		if err != nil {
			return fmt.Errorf("production_files_changed[%d]: %w", index, err)
		}
		if len(context.PermittedPaths) > 0 && !withinAnyPrefix(clean, context.PermittedPaths) {
			return fmt.Errorf("production_files_changed[%d] %q is outside permitted paths", index, path)
		}
		if context.WorktreeObserved {
			if _, exists := observed[clean]; !exists {
				return fmt.Errorf("production_files_changed[%d] %q was not observed in the worktree", index, path)
			}
		}
		if context.WorktreePath != "" {
			candidate := filepath.Join(context.WorktreePath, filepath.FromSlash(clean))
			relative, err := filepath.Rel(context.WorktreePath, candidate)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return fmt.Errorf("production_files_changed[%d] %q escapes worktree", index, path)
			}
		}
	}
	if len(value.FocusedCommands) == 0 {
		return errors.New("completed handoff focused_commands is required")
	}
	if len(value.FocusedCommands) > maxFocusedCommands {
		return fmt.Errorf("focused_commands exceeds %d entries", maxFocusedCommands)
	}
	for index, command := range value.FocusedCommands {
		if err := validateText(fmt.Sprintf("focused_commands[%d]", index), command, maxTextRunes, true); err != nil {
			return err
		}
	}
	if len(value.KnownLimitations) > maxKnownLimitations {
		return fmt.Errorf("known_limitations exceeds %d entries", maxKnownLimitations)
	}
	for index, limitation := range value.KnownLimitations {
		if err := validateText(fmt.Sprintf("known_limitations[%d]", index), limitation, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// isTestReport identifies the coordinator-owned test-stage envelope.
func isTestReport(value Report) bool {
	return value.Role == "test" && value.Stage == "test"
}

// isReviewReport identifies the coordinator-owned independent review envelope.
func isReviewReport(value Report) bool {
	return value.Role == "spec_review" && value.Stage == "review"
}

// ReviewFindingBlocks reports whether a finding is a concrete readiness blocker.
// Taste and scope observations remain visible advisories even when mislabeled
// with blocker severity, because they do not identify a correctness violation.
func ReviewFindingBlocks(value ReviewFinding) bool {
	if value.Severity != ReviewSeverityBlocker {
		return false
	}
	switch value.Category {
	case ReviewCategoryCorrectness, ReviewCategorySecurity, ReviewCategorySpecification, ReviewCategoryStandards, ReviewCategoryDocumentedStandards:
		return true
	default:
		return false
	}
}

// validateReviewHandoff validates the exact checkpoint binding and complete
// finding contract before a review can influence coordinator readiness.
func validateReviewHandoff(value ReviewHandoff, context ValidationContext) error {
	if err := validateSHA("review handoff reviewed_sha", value.ReviewedSHA); err != nil {
		return err
	}
	if context.CheckpointSHA != "" && value.ReviewedSHA != context.CheckpointSHA {
		return fmt.Errorf("reviewed SHA %q does not match checkpoint %q", value.ReviewedSHA, context.CheckpointSHA)
	}
	if len(value.Findings) > maxReviewFindings {
		return fmt.Errorf("review handoff findings exceeds %d entries", maxReviewFindings)
	}
	for index, finding := range value.Findings {
		fields := []struct {
			name  string
			value string
		}{
			{fmt.Sprintf("review finding[%d].location", index), finding.Location},
			{fmt.Sprintf("review finding[%d].claim", index), finding.Claim},
			{fmt.Sprintf("review finding[%d].evidence", index), finding.Evidence},
			{fmt.Sprintf("review finding[%d].suggested_resolution", index), finding.SuggestedResolution},
			{fmt.Sprintf("review finding[%d].suggested_owner", index), finding.SuggestedOwner},
		}
		for _, field := range fields {
			if err := validateText(field.name, field.value, maxTextRunes, true); err != nil {
				return err
			}
		}
		switch finding.Severity {
		case ReviewSeverityBlocker, ReviewSeverityAdvisory:
		default:
			return fmt.Errorf("review finding[%d].severity %q is unsupported", index, finding.Severity)
		}
		switch finding.Category {
		case ReviewCategoryCorrectness, ReviewCategorySecurity, ReviewCategorySpecification, ReviewCategoryStandards, ReviewCategoryDocumentedStandards, ReviewCategoryTaste, ReviewCategoryScope:
		default:
			return fmt.Errorf("review finding[%d].category %q is unsupported", index, finding.Category)
		}
	}
	return nil
}

// validateSHA accepts the full hexadecimal commit identities used by GitHub.
func validateSHA(field, value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("%s must contain exactly 40 or 64 lowercase hexadecimal characters", field)
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return fmt.Errorf("%s must contain exactly 40 or 64 lowercase hexadecimal characters", field)
		}
	}
	return nil
}

// hasExemption reports whether one fixed exemption category is present.
func hasExemption(values []Exemption, wanted Exemption) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// validateTestHandoff checks test ownership, observed paths, and the evidence
// required before the implementation role receives protected tests.
func validateTestHandoff(value TestHandoff, context ValidationContext) error {
	if len(value.AcceptanceCoverage) == 0 {
		return errors.New("test handoff acceptance_coverage is required")
	}
	if len(value.AcceptanceCoverage) > maxAcceptanceMappings {
		return fmt.Errorf("test handoff acceptance_coverage exceeds %d entries", maxAcceptanceMappings)
	}
	for index, mapping := range value.AcceptanceCoverage {
		if err := validateText(fmt.Sprintf("test acceptance_coverage[%d].criterion", index), mapping.Criterion, maxTextRunes, true); err != nil {
			return err
		}
		if err := validateText(fmt.Sprintf("test acceptance_coverage[%d].evidence", index), mapping.Evidence, maxTextRunes, true); err != nil {
			return err
		}
	}
	if len(value.ChangedFiles) == 0 {
		return errors.New("test handoff changed_files is required")
	}
	if len(value.ChangedFiles) > maxTestFiles {
		return fmt.Errorf("test handoff changed_files exceeds %d entries", maxTestFiles)
	}
	observed := make(map[string]struct{}, len(context.ObservedChanges))
	for _, path := range context.ObservedChanges {
		clean, err := cleanRelativePath(path)
		if err != nil {
			return fmt.Errorf("observed worktree path %q: %w", path, err)
		}
		observed[clean] = struct{}{}
		if len(context.PermittedPaths) > 0 && !withinAnyPrefix(clean, context.PermittedPaths) {
			return fmt.Errorf("observed worktree path %q is outside permitted paths", path)
		}
	}
	changed := make(map[string]struct{}, len(value.ChangedFiles))
	for index, path := range value.ChangedFiles {
		clean, err := validateTestPath(fmt.Sprintf("test changed_files[%d]", index), path, context)
		if err != nil {
			return err
		}
		if _, exists := changed[clean]; exists {
			return fmt.Errorf("test handoff changed_files[%d] %q is duplicated", index, path)
		}
		changed[clean] = struct{}{}
		if len(context.PermittedPaths) > 0 && !withinAnyPrefix(clean, context.PermittedPaths) {
			return fmt.Errorf("test changed_files[%d] %q is outside permitted paths", index, path)
		}
		if context.WorktreeObserved {
			if _, exists := observed[clean]; !exists {
				return fmt.Errorf("test changed_files[%d] %q was not observed in the worktree", index, path)
			}
		}
	}
	if context.WorktreeObserved {
		for path := range observed {
			if _, exists := changed[path]; !exists {
				return fmt.Errorf("test handoff omitted observed changed path %q", path)
			}
		}
	}
	if err := validateText("test handoff focused_test_command", value.FocusedTestCommand, maxTextRunes, true); err != nil {
		return err
	}
	if err := validateText("test handoff expected_failure_reason", value.ExpectedFailureReason, maxTextRunes, true); err != nil {
		return err
	}
	if len(value.ObservedFailureEvidence) == 0 {
		return errors.New("test handoff observed_failure_evidence is required")
	}
	if err := validateEvidence(value.ObservedFailureEvidence); err != nil {
		return fmt.Errorf("test handoff: %w", err)
	}
	if len(value.InfrastructureChanges) > maxTestFiles {
		return fmt.Errorf("test handoff infrastructure_changes exceeds %d entries", maxTestFiles)
	}
	for index, path := range value.InfrastructureChanges {
		clean, err := validateTestPath(fmt.Sprintf("test infrastructure_changes[%d]", index), path, context)
		if err != nil {
			return err
		}
		if _, exists := changed[clean]; !exists {
			return fmt.Errorf("test infrastructure_changes[%d] %q is not in changed_files", index, path)
		}
	}
	if len(value.UncoveredCriteria) > maxUncoveredCriteria {
		return fmt.Errorf("test handoff uncovered_criteria exceeds %d entries", maxUncoveredCriteria)
	}
	for index, criterion := range value.UncoveredCriteria {
		if err := validateText(fmt.Sprintf("test uncovered_criteria[%d]", index), criterion, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// validateTestPath applies the report-layer safe default test ownership rules;
// repository-specific prefixes are supplied through the validation context.
func validateTestPath(field, path string, context ValidationContext) (string, error) {
	if err := validateText(field, path, maxPathRunes, true); err != nil {
		return "", err
	}
	clean, err := cleanRelativePath(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	if !context.deferTestPathPolicy && !isTestOwnedPath(clean, context) {
		return "", fmt.Errorf("%s %q is outside configured test paths", field, path)
	}
	return clean, nil
}

// isTestOwnedPath recognizes conventional test paths and explicit repository
// policy prefixes without granting the test role the whole checkout.
func isTestOwnedPath(path string, context ValidationContext) bool {
	if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "test/") || strings.HasPrefix(path, "tests/") || strings.HasPrefix(path, "test-support/") || strings.HasPrefix(path, "__tests__/") {
		return true
	}
	// Filter out root-level entries that would grant access to the entire repository.
	combined := append(append([]string(nil), context.TestPaths...), context.TestInfrastructurePaths...)
	filtered := make([]string, 0, len(combined))
	for _, prefix := range combined {
		trimmed := strings.TrimSpace(prefix)
		if trimmed != "." && trimmed != "" {
			filtered = append(filtered, prefix)
		}
	}
	return withinAnyPrefix(path, filtered)
}

// ValidatePermittedPaths validates the repository-relative prefixes stored
// with an invocation and reused when its report is accepted.
func ValidatePermittedPaths(values []string) error {
	if len(values) > maxPermittedPaths {
		return fmt.Errorf("permitted_paths exceeds %d entries", maxPermittedPaths)
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "." {
			continue
		}
		if err := validateText(fmt.Sprintf("permitted_paths[%d]", index), value, maxPathRunes, true); err != nil {
			return err
		}
		if _, err := cleanRelativePath(value); err != nil {
			return fmt.Errorf("permitted_paths[%d]: %w", index, err)
		}
	}
	return nil
}

// validateQuestions ensures clarification identifiers are unique and safe.
func validateQuestions(values []Question) error {
	if len(values) > maxQuestions {
		return fmt.Errorf("questions exceeds %d entries", maxQuestions)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if !safeIdentifier(value.ID) || strings.TrimSpace(value.ID) == "" {
			return fmt.Errorf("questions[%d].id is required and must be safe", index)
		}
		if _, exists := seen[value.ID]; exists {
			return fmt.Errorf("questions[%d].id %q is duplicated", index, value.ID)
		}
		seen[value.ID] = struct{}{}
		if err := validateText(fmt.Sprintf("questions[%d].prompt", index), value.Prompt, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// validateEvidence ensures cannot-proceed evidence remains concise and
// observable rather than becoming a transcript transport.
func validateEvidence(values []Evidence) error {
	if len(values) > maxEvidenceEntries {
		return fmt.Errorf("evidence exceeds %d entries", maxEvidenceEntries)
	}
	for index, value := range values {
		if err := validateText(fmt.Sprintf("evidence[%d].kind", index), value.Kind, maxSummaryRunes, true); err != nil {
			return err
		}
		if err := validateText(fmt.Sprintf("evidence[%d].detail", index), value.Detail, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// validateInvocationDirectory ensures a result path is scoped to one safe
// invocation identifier and cannot silently target a parent directory.
func validateInvocationDirectory(path, invocationID string) error {
	if !filepath.IsAbs(path) {
		return errors.New("invocation result directory must be absolute")
	}
	if !safeIdentifier(invocationID) {
		return errors.New("invocation id contains unsafe characters")
	}
	if filepath.Base(filepath.Clean(path)) != invocationID {
		return errors.New("invocation result directory must be named for the invocation")
	}
	return nil
}

// cleanRelativePath normalizes and validates one repository-relative path.
func cleanRelativePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\x00\r\n\\") || filepath.IsAbs(path) {
		return "", errors.New("path must be a safe repository-relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", errors.New("path escapes the repository or targets Git metadata")
	}
	return clean, nil
}

// withinAnyPrefix reports whether path is equal to or contained below one
// repository-relative permitted prefix.
func withinAnyPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) == "." {
			return true
		}
		clean, err := cleanRelativePath(prefix)
		if err != nil {
			continue
		}
		if path == clean || strings.HasPrefix(path, clean+"/") {
			return true
		}
	}
	return false
}

// safeIdentifier reports whether a value can be used as a protocol identity or
// a single path component.
func safeIdentifier(value string) bool {
	if value == "" || value == "." || value == ".." || utf8.RuneCountInString(value) > maxIdentifierRunes {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

// identityFrom derives validation expectations from a report envelope so all
// public read and write paths apply the same identity binding rules.
func identityFrom(value Report, invocationID string) ValidationContext {
	return ValidationContext{
		InvocationID:        invocationID,
		RunID:               value.RunID,
		Harness:             value.Harness,
		Role:                value.Role,
		Stage:               value.Stage,
		deferTestPathPolicy: true,
	}
}

// validateText enforces a bounded, single-line protocol field.
func validateText(field, value string, maxRunes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be a nonempty single line", field)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s must be a nonempty single line", field)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s exceeds %d characters", field, maxRunes)
	}
	return nil
}
