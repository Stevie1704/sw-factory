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
)

// SchemaVersion is the version of the agent report envelope.
const SchemaVersion = 1

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
	// Questions are required only for a clarification outcome.
	Questions []Question `json:"questions,omitempty"`
	// Evidence is required only for a cannot-proceed outcome.
	Evidence []Evidence `json:"evidence,omitempty"`
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
	// WorktreePath is the host checkout root, when filesystem containment is checked.
	WorktreePath string
	// PermittedPaths contains repository-relative prefixes the role may report.
	PermittedPaths []string
	// ObservedChanges contains coordinator-observed repository-relative changes.
	ObservedChanges []string
	// WorktreeObserved distinguishes an inspected clean worktree from a
	// coordinator that has no worktree-inspection capability.
	WorktreeObserved bool
}

// WriteAtomic validates and publishes a report as report.json below the
// invocation-specific result directory. A temporary file is synced and
// renamed so readers never observe a partial JSON document.
func WriteAtomic(resultDirectory string, value Report) (string, error) {
	if err := Validate(value, ValidationContext{
		InvocationID: value.InvocationID,
		RunID:        value.RunID,
		Harness:      value.Harness,
		Role:         value.Role,
		Stage:        value.Stage,
	}); err != nil {
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
	if err := Validate(value, ValidationContext{
		InvocationID: invocationID,
		RunID:        value.RunID,
		Harness:      value.Harness,
		Role:         value.Role,
		Stage:        value.Stage,
	}); err != nil {
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
	if info, err := os.Lstat(path); err != nil {
		return Report{}, fmt.Errorf("inspect agent report: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return Report{}, errors.New("agent report path must not be a symbolic link")
	} else if !info.Mode().IsRegular() {
		return Report{}, errors.New("agent report path must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read agent report: %w", err)
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
	if err := Validate(value, ValidationContext{
		InvocationID: value.InvocationID,
		RunID:        value.RunID,
		Harness:      value.Harness,
		Role:         value.Role,
		Stage:        value.Stage,
	}); err != nil {
		return Report{}, err
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
	if strings.TrimSpace(value.Summary) == "" || strings.ContainsAny(value.Summary, "\x00\r\n") {
		return errors.New("report summary must be a nonempty single line")
	}
	if value.ReportedAt.IsZero() || value.ReportedAt.Location() == nil {
		return errors.New("report reported_at is required")
	}
	switch value.Outcome {
	case OutcomeCompleted:
		if value.Handoff == nil {
			return errors.New("completed report requires a handoff")
		}
		if len(value.Questions) != 0 || len(value.Evidence) != 0 {
			return errors.New("completed report must contain only a handoff")
		}
		if err := validateHandoff(*value.Handoff, context); err != nil {
			return err
		}
	case OutcomeNeedsClarification:
		if len(value.Questions) == 0 {
			return errors.New("needs_clarification report requires at least one question")
		}
		if value.Handoff != nil || len(value.Evidence) != 0 {
			return errors.New("needs_clarification report must contain only questions")
		}
		if err := validateQuestions(value.Questions); err != nil {
			return err
		}
	case OutcomeCannotProceed:
		if len(value.Evidence) == 0 {
			return errors.New("cannot_proceed report requires evidence")
		}
		if value.Handoff != nil || len(value.Questions) != 0 {
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
	return nil
}

// validateHandoff checks handoff text, path safety, permitted prefixes, and
// agreement with the coordinator's observed worktree changes.
func validateHandoff(value Handoff, context ValidationContext) error {
	if strings.TrimSpace(value.ChangeSummary) == "" || strings.ContainsAny(value.ChangeSummary, "\x00\r\n") {
		return errors.New("completed handoff change_summary is required")
	}
	if len(value.AcceptanceMapping) == 0 {
		return errors.New("completed handoff acceptance_mapping is required")
	}
	for index, mapping := range value.AcceptanceMapping {
		if strings.TrimSpace(mapping.Criterion) == "" || strings.TrimSpace(mapping.Evidence) == "" || strings.ContainsAny(mapping.Criterion+mapping.Evidence, "\x00\r\n") {
			return fmt.Errorf("acceptance_mapping[%d] requires criterion and evidence", index)
		}
	}
	observed := make(map[string]struct{}, len(context.ObservedChanges))
	for _, path := range context.ObservedChanges {
		if clean, err := cleanRelativePath(path); err == nil {
			observed[clean] = struct{}{}
		}
	}
	for index, path := range value.ProductionFilesChanged {
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
	for index, command := range value.FocusedCommands {
		if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\x00\r\n") {
			return fmt.Errorf("focused_commands[%d] must be a nonempty single line", index)
		}
	}
	for index, limitation := range value.KnownLimitations {
		if strings.TrimSpace(limitation) == "" || strings.ContainsAny(limitation, "\x00\r\n") {
			return fmt.Errorf("known_limitations[%d] must be a nonempty single line", index)
		}
	}
	return nil
}

// ValidatePermittedPaths validates the repository-relative prefixes stored
// with an invocation and reused when its report is accepted.
func ValidatePermittedPaths(values []string) error {
	for index, value := range values {
		if _, err := cleanRelativePath(value); err != nil {
			return fmt.Errorf("permitted_paths[%d]: %w", index, err)
		}
	}
	return nil
}

// validateQuestions ensures clarification identifiers are unique and safe.
func validateQuestions(values []Question) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if !safeIdentifier(value.ID) || strings.TrimSpace(value.ID) == "" {
			return fmt.Errorf("questions[%d].id is required and must be safe", index)
		}
		if _, exists := seen[value.ID]; exists {
			return fmt.Errorf("questions[%d].id %q is duplicated", index, value.ID)
		}
		seen[value.ID] = struct{}{}
		if strings.TrimSpace(value.Prompt) == "" || strings.ContainsAny(value.Prompt, "\x00\r\n") {
			return fmt.Errorf("questions[%d].prompt must be a nonempty single line", index)
		}
	}
	return nil
}

// validateEvidence ensures cannot-proceed evidence remains concise and
// observable rather than becoming a transcript transport.
func validateEvidence(values []Evidence) error {
	for index, value := range values {
		if strings.TrimSpace(value.Kind) == "" || strings.TrimSpace(value.Detail) == "" {
			return fmt.Errorf("evidence[%d] requires kind and detail", index)
		}
		if strings.ContainsAny(value.Kind+value.Detail, "\x00\r\n") {
			return fmt.Errorf("evidence[%d] must not contain control characters", index)
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
	if value == "" || value == "." || value == ".." {
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
