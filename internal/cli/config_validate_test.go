package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

// configValidateDocument is the JSON shape config validate writes.
type configValidateDocument struct {
	SchemaVersion int    `json:"schema_version"`
	File          string `json:"file"`
	Valid         bool   `json:"valid"`
	Completeness  string `json:"completeness"`
	Diagnostics   []struct {
		Code  string `json:"code"`
		Field string `json:"field"`
	} `json:"diagnostics"`
}

// runConfigValidate runs config validate against contents without a host
// configuration and returns the exit status and the decoded document.
func runConfigValidate(t *testing.T, contents string) (int, configValidateDocument) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTORY_CONFIG", filepath.Join(t.TempDir(), "absent", "config.yaml"))
	var output, errorsOutput bytes.Buffer
	status := cli.Run(context.Background(), []string{"config", "validate", "--file", path, "--json"}, &output, &errorsOutput)
	var document configValidateDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("output is not one JSON document: %v: %q (stderr %q)", err, output.String(), errorsOutput.String())
	}
	if document.SchemaVersion != 1 || document.File != path {
		t.Fatalf("document header = %#v, want schema 1 for %q", document, path)
	}
	return status, document
}

// TestConfigValidateAcceptsAValidPolicyWithoutHostConfiguration verifies a
// valid policy passes with no registration present.
func TestConfigValidateAcceptsAValidPolicyWithoutHostConfiguration(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	status, document := runConfigValidate(t, string(contents))

	if status != 0 || !document.Valid || document.Completeness != "complete" || len(document.Diagnostics) != 0 {
		t.Fatalf("status = %d, document = %#v, want a complete valid result", status, document)
	}
}

// TestConfigValidateReportsAnInvalidPolicy verifies an invalid policy exits 1
// with a typed diagnostic.
func TestConfigValidateReportsAnInvalidPolicy(t *testing.T) {
	status, document := runConfigValidate(t, "schema_version: 1\nunexpected: true\n")

	if status != 1 || document.Valid || document.Completeness != "not_run" {
		t.Fatalf("status = %d, document = %#v, want an invalid result", status, document)
	}
	if len(document.Diagnostics) != 1 || document.Diagnostics[0].Code != "unknown_field" || document.Diagnostics[0].Field != "unexpected" {
		t.Fatalf("diagnostics = %#v, want one unknown_field for unexpected", document.Diagnostics)
	}
}

// TestConfigValidateRefusesIncompleteArguments verifies usage errors exit 2.
func TestConfigValidateRefusesIncompleteArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand": {"config"},
		"unknown":       {"config", "check"},
		"no file":       {"config", "validate", "--json"},
		"no json":       {"config", "validate", "--file", "factory.yaml"},
	} {
		var output, errorsOutput bytes.Buffer
		if status := cli.Run(context.Background(), args, &output, &errorsOutput); status != 2 {
			t.Errorf("%s: status = %d, want 2", name, status)
		}
		if !strings.Contains(errorsOutput.String(), "error") && !strings.Contains(errorsOutput.String(), "Usage") {
			t.Errorf("%s: stderr = %q, want an explanation", name, errorsOutput.String())
		}
	}
}
