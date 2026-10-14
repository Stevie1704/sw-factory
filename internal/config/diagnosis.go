package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// DiagnosticCode is the stable machine-readable class of one repository
// configuration diagnostic.
type DiagnosticCode string

const (
	// DiagnosticFileUnreadable means the file could not be read.
	DiagnosticFileUnreadable DiagnosticCode = "file_unreadable"
	// DiagnosticSyntax means the file is not exactly one YAML document.
	DiagnosticSyntax DiagnosticCode = "syntax_error"
	// DiagnosticUnknownField means the strict parser found a field that the
	// supported schema does not declare.
	DiagnosticUnknownField DiagnosticCode = "unknown_field"
	// DiagnosticTypeMismatch means a value has the wrong YAML type for its field.
	DiagnosticTypeMismatch DiagnosticCode = "type_mismatch"
	// DiagnosticUnsupportedSchema means schema_version is newer than this build.
	DiagnosticUnsupportedSchema DiagnosticCode = "unsupported_schema"
	// DiagnosticRepositoryPolicy means the file crosses the boundary of
	// factory-owned workflow policy.
	DiagnosticRepositoryPolicy DiagnosticCode = "repository_policy"
	// DiagnosticInvalidValue means a semantic validation rule failed.
	DiagnosticInvalidValue DiagnosticCode = "invalid_value"
)

// ValidationCompleteness tells how far validation got, so a caller knows
// whether the absence of more diagnostics means anything.
type ValidationCompleteness string

const (
	// ValidationComplete means every check ran and passed.
	ValidationComplete ValidationCompleteness = "complete"
	// ValidationFirstError means semantic validation stopped at its first
	// failure; later fields were not checked.
	ValidationFirstError ValidationCompleteness = "first_error"
	// ValidationNotRun means the file did not parse, so no semantic check ran.
	ValidationNotRun ValidationCompleteness = "not_run"
)

// Diagnostic is one finding about a repository configuration file. It never
// contains a value from the file, because a value can carry a secret.
type Diagnostic struct {
	// Code is the stable class of the finding.
	Code DiagnosticCode `json:"code"`
	// Field is the configuration field path, when it is known.
	Field string `json:"field,omitempty"`
	// Line is the 1-based line in the file, when the parser reports one.
	Line int `json:"line,omitempty"`
	// Message explains the finding in fixed text.
	Message string `json:"message"`
}

// RepositoryDiagnosis is the result of validating one repository
// configuration file with the production parser and validator.
type RepositoryDiagnosis struct {
	// Valid reports that the file loads exactly as a run would load it.
	Valid bool `json:"valid"`
	// Completeness tells how far validation got.
	Completeness ValidationCompleteness `json:"completeness"`
	// Diagnostics holds the findings; it is empty for a valid file.
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// errMultipleDocuments refuses a file with more than one YAML document.
var errMultipleDocuments = errors.New("configuration contains more than one YAML document")

// yamlLine finds the line number in a YAML parser message.
var yamlLine = regexp.MustCompile(`line (\d+):`)

// yamlUnknownField finds the field name in a strict-decoding message.
var yamlUnknownField = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)

// safeFieldName matches a field name that is safe to repeat in a diagnostic.
var safeFieldName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// DiagnoseRepositoryFile loads path with LoadRepository and classifies the
// result. It needs no host configuration, Docker, or network. Semantic
// validation reports only its first failure.
func DiagnoseRepositoryFile(path string) RepositoryDiagnosis {
	_, err := LoadRepository(path)
	if err == nil {
		return RepositoryDiagnosis{Valid: true, Completeness: ValidationComplete, Diagnostics: []Diagnostic{}}
	}
	var validationErr *ValidationError
	var policyErr *PolicyError
	var schemaErr *UnknownSchemaVersionError
	var fileErr *ConfigFileError
	switch {
	case errors.As(err, &validationErr):
		return firstError(Diagnostic{Code: DiagnosticInvalidValue, Field: validationErr.Field, Message: validationErr.Message})
	case errors.As(err, &policyErr):
		diagnostic := Diagnostic{Code: DiagnosticRepositoryPolicy, Field: policyErr.Field, Message: policyErr.Message}
		if _, workflowField := factoryOwnedWorkflowFields[policyErr.Field]; workflowField {
			return notRun(diagnostic)
		}
		return firstError(diagnostic)
	case errors.As(err, &schemaErr):
		return notRun(Diagnostic{Code: DiagnosticUnsupportedSchema, Field: "schema_version", Message: fmt.Sprintf("schema_version %d is newer than supported version %d", schemaErr.Version, schemaErr.Supported)})
	case errors.As(err, &fileErr):
		return notRun(decodeDiagnostics(fileErr.Err)...)
	default:
		return notRun(Diagnostic{Code: DiagnosticSyntax, Message: "the file could not be decoded"})
	}
}

// firstError wraps one semantic diagnostic.
func firstError(diagnostic Diagnostic) RepositoryDiagnosis {
	return RepositoryDiagnosis{Completeness: ValidationFirstError, Diagnostics: []Diagnostic{diagnostic}}
}

// notRun wraps diagnostics that stopped validation before semantic checks.
func notRun(diagnostics ...Diagnostic) RepositoryDiagnosis {
	return RepositoryDiagnosis{Completeness: ValidationNotRun, Diagnostics: diagnostics}
}

// decodeDiagnostics classifies a read or decode failure. It keeps only line
// numbers and field names from parser messages, because the messages can
// quote values from the file.
func decodeDiagnostics(err error) []Diagnostic {
	var pathErr *fs.PathError
	var typeErr *yaml.TypeError
	switch {
	case errors.As(err, &pathErr):
		return []Diagnostic{{Code: DiagnosticFileUnreadable, Message: "the file cannot be read: " + pathErr.Err.Error()}}
	case errors.Is(err, io.EOF):
		return []Diagnostic{{Code: DiagnosticSyntax, Message: "the file contains no YAML document"}}
	case errors.Is(err, errMultipleDocuments):
		return []Diagnostic{{Code: DiagnosticSyntax, Message: "the file contains more than one YAML document"}}
	case errors.As(err, &typeErr):
		diagnostics := make([]Diagnostic, 0, len(typeErr.Errors))
		for _, message := range typeErr.Errors {
			diagnostics = append(diagnostics, typeDiagnostic(message))
		}
		return diagnostics
	default:
		return []Diagnostic{{Code: DiagnosticSyntax, Line: parserLine(err.Error()), Message: "the file is not valid YAML"}}
	}
}

// typeDiagnostic classifies one strict-decoding message as an unknown field
// or a type mismatch.
func typeDiagnostic(message string) Diagnostic {
	if match := yamlUnknownField.FindStringSubmatch(message); match != nil {
		diagnostic := Diagnostic{Code: DiagnosticUnknownField, Line: parserLine(message), Message: "the field is not declared by the supported schema"}
		if safeFieldName.MatchString(match[2]) {
			diagnostic.Field = match[2]
		}
		return diagnostic
	}
	return Diagnostic{Code: DiagnosticTypeMismatch, Line: parserLine(message), Message: "the value has the wrong type for its field"}
}

// parserLine returns the first line number in a parser message, or zero.
func parserLine(message string) int {
	match := yamlLine.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}
