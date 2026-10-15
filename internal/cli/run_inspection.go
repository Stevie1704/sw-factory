package cli

import (
	"context"
	"errors"
	"io"

	"github.com/Stevie1704/sw-factory/internal/factory"
)

// inspectionDocument is the versioned envelope of local read-only commands.
type inspectionDocument struct {
	SchemaVersion int              `json:"schema_version"`
	Command       string           `json:"command"`
	Outcome       string           `json:"outcome"`
	Result        any              `json:"result,omitempty"`
	Error         *inspectionError `json:"error,omitempty"`
}

// inspectionError exposes fixed codes and messages, never underlying store content.
type inspectionError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// runRuns validates and reads a bounded run-list page.
func runRuns(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("runs", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	asJSON := flags.Bool("json", false, "write one schema-versioned JSON document")
	offset := flags.Int("offset", 0, "zero-based row offset (ordering: updated_at DESC, id ASC)")
	limit := flags.Int("limit", factory.DefaultRunPageSize, "maximum rows (1-100)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *offset < 0 || *limit < 1 || *limit > factory.MaxRunPageSize {
		writeError(errorsOutput, errors.New("runs requires offset >= 0, limit 1-100, and no positional arguments"))
		return 2
	}
	result, err := newService(*configPath).Runs(ctx, *offset, *limit)
	return writeInspection(output, errorsOutput, *asJSON, "runs", result, err)
}

// writeInspection writes a result or a stable, content-free error document.
func writeInspection(output, errorsOutput io.Writer, asJSON bool, command string, result any, err error) int {
	doc := inspectionDocument{SchemaVersion: 1, Command: command, Outcome: "ok", Result: result}
	if err != nil {
		doc.Outcome = "error"
		doc.Result = nil
		code, message := "inspection.unavailable", "Local inspection is unavailable; check configuration and operational-store readiness."
		if errors.Is(err, factory.ErrRunNotFound) {
			code, message = "run.not_found", "No retained run or evaluation summary has that identity."
		}
		if errors.Is(err, factory.ErrEvidenceNotFound) {
			code, message = "evidence.not_found", "No allowlisted evidence has that identity for this run."
		}
		if errors.Is(err, factory.ErrEvidenceChanged) {
			code, message = "evidence.changed", "The selected evidence changed; inspect the run again before retrieving content."
		}
		if errors.Is(err, factory.ErrEvidenceUnavailable) {
			code, message = "evidence.unavailable", "The selected evidence is unreadable or has an unsafe file or recorded ownership identity."
		}
		doc.Error = &inspectionError{Code: code, Message: message}
	}
	if asJSON {
		if !writeJSONDocument(output, errorsOutput, doc) {
			return 1
		}
	} else if err != nil {
		writeError(errorsOutput, errors.New(doc.Error.Message))
	} else if !writeJSONDocument(output, errorsOutput, doc) {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

// runRun dispatches the named-run inspection commands.
func runRun(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "evidence" {
		return runRunEvidence(ctx, args[1:], defaultConfigPath, output, errorsOutput)
	}
	if len(args) == 0 || args[0] != "inspect" {
		writeError(errorsOutput, errors.New("run requires inspect or evidence; use 'factory run inspect -h' or 'factory run evidence -h'"))
		return 2
	}
	flags := newFlagSet("run inspect", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	asJSON := flags.Bool("json", false, "write one schema-versioned JSON document")
	runID := flags.String("run-id", "", "required persisted run identity")
	offset := flags.Int("offset", 0, "zero-based offset in each retained record collection")
	limit := flags.Int("limit", factory.DefaultRunPageSize, "maximum records per collection (1-100)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *runID == "" || *offset < 0 || *limit < 1 || *limit > factory.MaxRunPageSize {
		writeError(errorsOutput, errors.New("run inspect requires --run-id, offset >= 0, limit 1-100, and no positional arguments"))
		return 2
	}
	result, err := newService(*configPath).RunInspect(ctx, *runID, *offset, *limit)
	return writeInspection(output, errorsOutput, *asJSON, "run inspect", result, err)
}

// runRunEvidence retrieves one explicitly selected bounded local byte window.
func runRunEvidence(ctx context.Context, args []string, defaultConfigPath string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("run evidence", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	asJSON := flags.Bool("json", false, "write one schema-versioned JSON document")
	runID := flags.String("run-id", "", "required persisted run identity")
	evidenceID := flags.String("evidence-id", "", "required evidence identity returned by run inspect; never a path")
	offset := flags.Int("offset", 0, "zero-based byte offset in the retained 256 KiB prefix")
	limit := flags.Int("limit", factory.DefaultEvidencePageSize, "maximum bytes to retrieve (1-65536)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *runID == "" || *evidenceID == "" || *offset < 0 || *limit < 1 || *limit > factory.MaxEvidencePageSize {
		writeError(errorsOutput, errors.New("run evidence requires --run-id, --evidence-id, offset >= 0, limit 1-65536, and no positional arguments"))
		return 2
	}
	result, err := newService(*configPath).RunEvidence(ctx, *runID, *evidenceID, *offset, *limit)
	return writeInspection(output, errorsOutput, *asJSON, "run evidence", result, err)
}
