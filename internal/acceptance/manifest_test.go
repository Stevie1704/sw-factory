package acceptance_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/acceptance"
)

// repositoryRoot is the module root relative to this package directory.
const repositoryRoot = "../.."

// TestEveryScenarioNamesExistingDeterministicTests keeps the manifest and the
// test suite together: a renamed or deleted test fails here instead of
// silently removing a scenario's CI coverage.
func TestEveryScenarioNamesExistingDeterministicTests(t *testing.T) {
	manifest, err := acceptance.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]map[string]bool{}
	for _, scenario := range manifest.Scenarios {
		for _, reference := range scenario.DeterministicTests {
			directory, name, _ := strings.Cut(reference, ":")
			if tests[directory] == nil {
				tests[directory] = testFunctions(t, filepath.Join(repositoryRoot, filepath.FromSlash(directory)))
			}
			if !tests[directory][name] {
				t.Errorf("scenario %q names %s, which does not exist", scenario.ID, reference)
			}
		}
	}
}

// releaseEvidenceTest matches a `<package directory>:<TestName>` reference in
// a release evidence record. The directory "." is the module root.
var releaseEvidenceTest = regexp.MustCompile("`" + `(\.|[a-z][a-z0-9_/-]*):(Test[A-Za-z0-9_]+)` + "`")

// TestCopilotReleaseEvidenceNamesExistingTests keeps the Copilot release
// traceability record honest: every deterministic test it cites as evidence
// for an acceptance criterion must exist.
func TestCopilotReleaseEvidenceNamesExistingTests(t *testing.T) {
	record, err := os.ReadFile(filepath.Join(repositoryRoot, "docs", "acceptance", "copilot.md"))
	if err != nil {
		t.Fatal(err)
	}
	references := releaseEvidenceTest.FindAllStringSubmatch(string(record), -1)
	if len(references) == 0 {
		t.Fatal("docs/acceptance/copilot.md names no deterministic test")
	}
	tests := map[string]map[string]bool{}
	for _, reference := range references {
		directory, name := reference[1], reference[2]
		if tests[directory] == nil {
			tests[directory] = testFunctions(t, filepath.Join(repositoryRoot, filepath.FromSlash(directory)))
		}
		if !tests[directory][name] {
			t.Errorf("docs/acceptance/copilot.md names %s:%s, which does not exist", directory, name)
		}
	}
}

// testFunctions returns the names of the top-level functions in the test
// files of one package directory.
func testFunctions(t *testing.T, directory string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
				names[function.Name.Name] = true
			}
		}
	}
	return names
}
