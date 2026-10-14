package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// validRepositoryYAML is a complete repository configuration that the strict
// parser and validator accept.
const validRepositoryYAML = `schema_version: 1
target_branch: main
setup: npm ci
setup_files: [package.json, package-lock.json]
setup_environment_policy: clean
gates:
  - name: lint
    command: npm run lint
    timeout: 5m
    blocking: true
    environment_policy: clean
role_harness_defaults:
  implementation: claude
model_options:
  implementation: [claude-opus-5]
timeouts:
  setup: 10m
  agent: 90m
  gate: 15m
  review: 30m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: advisory
worker_build:
  image: ghcr.io/example/project-worker
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`

// writeRepositoryFile writes contents to a new factory.yaml and returns its path.
func writeRepositoryFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDiagnoseRepositoryFileAcceptsAValidPolicy verifies a valid file reports
// no diagnostic and a complete validation.
func TestDiagnoseRepositoryFileAcceptsAValidPolicy(t *testing.T) {
	t.Parallel()

	got := config.DiagnoseRepositoryFile(writeRepositoryFile(t, validRepositoryYAML))
	if !got.Valid || got.Completeness != config.ValidationComplete || len(got.Diagnostics) != 0 {
		t.Fatalf("DiagnoseRepositoryFile() = %#v, want a complete valid diagnosis", got)
	}
}

// TestDiagnoseRepositoryFileClassifiesEachFailure verifies every failure class
// has its own stable code, a field path when one is known, and a completeness
// that tells whether semantic validation ran.
func TestDiagnoseRepositoryFileClassifiesEachFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		contents     string
		code         config.DiagnosticCode
		field        string
		line         int
		completeness config.ValidationCompleteness
	}{
		{
			name:         "syntax",
			contents:     "schema_version: 1\ngates: [\n",
			code:         config.DiagnosticSyntax,
			completeness: config.ValidationNotRun,
		},
		{
			name:         "empty file",
			contents:     "",
			code:         config.DiagnosticSyntax,
			completeness: config.ValidationNotRun,
		},
		{
			name:         "several documents",
			contents:     validRepositoryYAML + "---\nschema_version: 1\n",
			code:         config.DiagnosticSyntax,
			completeness: config.ValidationNotRun,
		},
		{
			name:         "unknown field",
			contents:     validRepositoryYAML + "unexpected_field: true\n",
			code:         config.DiagnosticUnknownField,
			field:        "unexpected_field",
			line:         strings.Count(validRepositoryYAML, "\n") + 1,
			completeness: config.ValidationNotRun,
		},
		{
			name:         "wrong type",
			contents:     strings.Replace(validRepositoryYAML, "check_repair: 3", "check_repair: three", 1),
			code:         config.DiagnosticTypeMismatch,
			completeness: config.ValidationNotRun,
		},
		{
			name:         "newer schema with new fields",
			contents:     strings.Replace(validRepositoryYAML, "schema_version: 1", "schema_version: 2\nnew_field: true", 1),
			code:         config.DiagnosticUnsupportedSchema,
			field:        "schema_version",
			completeness: config.ValidationNotRun,
		},
		{
			name:         "factory-owned workflow field",
			contents:     validRepositoryYAML + "roles: []\n",
			code:         config.DiagnosticRepositoryPolicy,
			field:        "roles",
			completeness: config.ValidationNotRun,
		},
		{
			name:         "semantic",
			contents:     strings.Replace(validRepositoryYAML, "timeout: 5m", "timeout: soon", 1),
			code:         config.DiagnosticInvalidValue,
			field:        "gates[0].timeout",
			completeness: config.ValidationFirstError,
		},
		{
			name:         "undeclared role",
			contents:     strings.Replace(strings.Replace(validRepositoryYAML, "implementation: claude", "implementation: claude\n  custom: claude", 1), "implementation: [claude-opus-5]", "implementation: [claude-opus-5]\n  custom: [claude-opus-5]", 1),
			code:         config.DiagnosticRepositoryPolicy,
			completeness: config.ValidationFirstError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := config.DiagnoseRepositoryFile(writeRepositoryFile(t, test.contents))
			if got.Valid || got.Completeness != test.completeness || len(got.Diagnostics) == 0 {
				t.Fatalf("DiagnoseRepositoryFile() = %#v, want an invalid diagnosis with completeness %q", got, test.completeness)
			}
			first := got.Diagnostics[0]
			if first.Code != test.code {
				t.Fatalf("diagnostic code = %q, want %q (%#v)", first.Code, test.code, first)
			}
			if test.field != "" && first.Field != test.field {
				t.Fatalf("diagnostic field = %q, want %q", first.Field, test.field)
			}
			if test.line != 0 && first.Line != test.line {
				t.Fatalf("diagnostic line = %d, want %d", first.Line, test.line)
			}
			if first.Message == "" {
				t.Fatal("diagnostic message is empty")
			}
		})
	}
}

// TestDiagnoseRepositoryFileReportsAnUnreadableFile verifies a missing file is
// distinguished from a file that does not parse.
func TestDiagnoseRepositoryFileReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()

	got := config.DiagnoseRepositoryFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if got.Valid || got.Completeness != config.ValidationNotRun || len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != config.DiagnosticFileUnreadable {
		t.Fatalf("DiagnoseRepositoryFile() = %#v, want one file_unreadable diagnostic", got)
	}
}

// TestDiagnoseRepositoryFileNeverEchoesASuppliedValue verifies a value in the
// file, which can carry a secret, does not appear in any diagnostic.
func TestDiagnoseRepositoryFileNeverEchoesASuppliedValue(t *testing.T) {
	t.Parallel()

	const secret = "ghp_supersecretvalue"
	for name, contents := range map[string]string{
		"wrong type": strings.Replace(validRepositoryYAML, "check_repair: 3", "check_repair: "+secret, 1),
		"syntax":     "schema_version: 1\nsetup: \"" + secret + "\n  : [",
		"semantic":   strings.Replace(validRepositoryYAML, "timeout: 5m", "timeout: "+secret, 1),
	} {
		got := config.DiagnoseRepositoryFile(writeRepositoryFile(t, contents))
		for _, diagnostic := range got.Diagnostics {
			if strings.Contains(diagnostic.Message, secret) || strings.Contains(diagnostic.Field, secret) {
				t.Fatalf("%s: diagnostic %#v echoes the supplied value", name, diagnostic)
			}
		}
		if len(got.Diagnostics) == 0 {
			t.Fatalf("%s: no diagnostic", name)
		}
	}
}
