package localissues_test

import (
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalIssueSurvivesRename verifies identity and exact content through the public format seam.
func TestCanonicalIssueSurvivesRename(t *testing.T) {
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-00112233445566778899aabbccddeeff", Title: "Add export", Kind: localissues.KindTask, Correlation: "session/1/export", Body: "## Problem\n\nUsers need exports.\n\n## Scope and non-goals\n\nExport CSV; PDF is excluded.\n\n## Acceptance criteria\n\n- CSV downloads.\n"}
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := localissues.Parse("docs/issues/renamed.md", data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != issue.ID || parsed.Body != issue.Body || parsed.Path != "docs/issues/renamed.md" {
		t.Fatalf("parsed = %#v", parsed)
	}

	again, err := localissues.Render(parsed)
	if err != nil || string(again) != string(data) {
		t.Fatalf("render is not canonical: %v", err)
	}
}

// TestMissingRequiredFrontMatterCannotImportAnIssue rejects a partial schema rather than inferring defaults.
func TestMissingRequiredFrontMatterCannotImportAnIssue(t *testing.T) {
	source := "---\nschema_version: 1\nid: local-00112233445566778899aabbccddeeff\ntitle: Add export\nkind: task\npublication_correlation: session/1/export\n---\n\n## Problem\n\nExport needed.\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\nCSV downloads.\n"
	if _, err := localissues.Parse("docs/issues/export.md", []byte(source)); err == nil {
		t.Fatal("missing dependencies field accepted")
	}
}

// acceptedIssue is complete source content supplied through the public renderer.
func acceptedIssue(id, correlation string) localissues.Issue {
	return localissues.Issue{SchemaVersion: 1, ID: id, Title: "Add export", Kind: localissues.KindTask, Correlation: correlation, Body: "## Problem\n\nUsers need exports.\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\n- CSV downloads.\n"}
}

// fileFor renders one valid individual issue before graph validation.
func fileFor(t *testing.T, issue localissues.Issue) localissues.File {
	t.Helper()
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	name, err := localissues.DefaultPath(issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	return localissues.File{Path: name, Data: data, Mode: 0o644}
}

// TestGraphIdentitySurvivesRenameAndEnumerationOrder binds the complete graph rather than filenames.
func TestGraphIdentitySurvivesRenameAndEnumerationOrder(t *testing.T) {
	first := acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first")
	second := acceptedIssue("local-ffeeddccbbaa99887766554433221100", "session/1/second")
	second.Dependencies = []string{first.ID}
	a, b := fileFor(t, first), fileFor(t, second)
	original, err := localissues.Validate([]localissues.File{a, b})
	if err != nil {
		t.Fatal(err)
	}
	a.Path = "docs/issues/renamed.md"
	renamed, err := localissues.Validate([]localissues.File{b, a})
	if err != nil || renamed.Digest != original.Digest {
		t.Fatalf("rename changed graph identity: %v", err)
	}
	second.Body = strings.Replace(second.Body, "CSV downloads.", "CSV and JSON download.", 1)
	changed, err := localissues.Validate([]localissues.File{a, fileFor(t, second)})
	if err != nil || changed.Digest == original.Digest {
		t.Fatalf("content change did not change graph identity: %v", err)
	}
}

// TestInvalidSourcesCannotBecomeAcceptedIssues exercises strict decoding at the source seam.
func TestInvalidSourcesCannotBecomeAcceptedIssues(t *testing.T) {
	file := fileFor(t, acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first"))
	cases := []struct {
		name, path, data string
		mode             fs.FileMode
	}{
		{name: "unknown authority field", data: strings.Replace(string(file.Data), "schema_version: 1", "schema_version: 1\nauthorized_user: attacker", 1)},
		{name: "unsupported schema", data: strings.Replace(string(file.Data), "schema_version: 1", "schema_version: 99", 1)},
		{name: "duplicate field", data: strings.Replace(string(file.Data), "schema_version: 1", "schema_version: 1\nschema_version: 1", 1)},
		{name: "alias", data: strings.Replace(string(file.Data), "title: Add export", "title: &title Add export", 1)},
		{name: "bad title type", data: strings.Replace(string(file.Data), "title: Add export", "title: true", 1)},
		{name: "missing body section", data: strings.Replace(string(file.Data), "## Acceptance criteria", "## Notes", 1)},
		{name: "null dependencies", data: strings.Replace(string(file.Data), "dependencies: []", "dependencies: null", 1)},
		{name: "escaping path", path: "docs/issues/../escape.md", data: string(file.Data)},
		{name: "nested path", path: "docs/issues/nested/export.md", data: string(file.Data)},
		{name: "symlink", data: string(file.Data), mode: fs.ModeSymlink},
		{name: "directory", data: string(file.Data), mode: fs.ModeDir},
		{name: "invalid UTF-8", data: string(append(file.Data, 0xff))},
		{name: "oversized", data: string(file.Data) + strings.Repeat("x", localissues.MaxFileBytes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.path
			if name == "" {
				name = file.Path
			}
			if _, err := localissues.Validate([]localissues.File{{Path: name, Data: []byte(tc.data), Mode: tc.mode}}); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

// TestInvalidGraphsAreRejectedAsAWhole prevents partial indexing of inconsistent relationships.
func TestInvalidGraphsAreRejectedAsAWhole(t *testing.T) {
	first := acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first")
	second := acceptedIssue("local-ffeeddccbbaa99887766554433221100", "session/1/second")
	t.Run("duplicate identity", func(t *testing.T) {
		a, b := fileFor(t, first), fileFor(t, first)
		b.Path = "docs/issues/duplicate.md"
		if _, err := localissues.Validate([]localissues.File{a, b}); err == nil {
			t.Fatal("duplicate accepted")
		}
	})
	t.Run("duplicate correlation", func(t *testing.T) {
		second.Correlation = first.Correlation
		if _, err := localissues.Validate([]localissues.File{fileFor(t, first), fileFor(t, second)}); err == nil {
			t.Fatal("duplicate correlation accepted")
		}
	})
	second.Correlation = "session/1/second"
	t.Run("dangling", func(t *testing.T) {
		second.Dependencies = []string{first.ID}
		if _, err := localissues.Validate([]localissues.File{fileFor(t, second)}); err == nil {
			t.Fatal("dangling accepted")
		}
	})
	t.Run("dependency cycle", func(t *testing.T) {
		first.Dependencies = []string{second.ID}
		second.Dependencies = []string{first.ID}
		if _, err := localissues.Validate([]localissues.File{fileFor(t, first), fileFor(t, second)}); err == nil {
			t.Fatal("cycle accepted")
		}
	})
	first.Dependencies = nil
	second.Dependencies = nil
	t.Run("non-parent parent", func(t *testing.T) {
		second.Parent = first.ID
		if _, err := localissues.Validate([]localissues.File{fileFor(t, first), fileFor(t, second)}); err == nil {
			t.Fatal("task parent accepted")
		}
	})
	for _, kind := range []localissues.Kind{localissues.KindParent, localissues.KindInvestigation} {
		t.Run("dependency on "+string(kind), func(t *testing.T) {
			target, dependent := first, second
			target.Kind, target.Parent, dependent.Parent = kind, "", ""
			dependent.Dependencies = []string{target.ID}
			if _, err := localissues.Validate([]localissues.File{fileFor(t, target), fileFor(t, dependent)}); err == nil || !strings.Contains(err.Error(), "must reference a task") {
				t.Fatalf("dependency on a %s: %v", kind, err)
			}
		})
	}
	t.Run("parent cycle", func(t *testing.T) {
		first.Kind = localissues.KindParent
		second.Kind = localissues.KindParent
		first.Parent = second.ID
		second.Parent = first.ID
		if _, err := localissues.Validate([]localissues.File{fileFor(t, first), fileFor(t, second)}); err == nil {
			t.Fatal("parent cycle accepted")
		}
	})
}

// TestDirectoryPreflightRefusesSymlinksAndNeverWrites leaves the imported source untouched.
func TestDirectoryPreflightRefusesSymlinksAndNeverWrites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "issues")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := fileFor(t, acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first"))
	source := filepath.Join(root, filepath.FromSlash(file.Path))
	if err := os.WriteFile(source, file.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localissues.ReadDirectory(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || string(after) != string(file.Data) {
		t.Fatal("preflight changed source")
	}
	if err := os.Symlink(source, filepath.Join(dir, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := localissues.ReadDirectory(root); err == nil {
		t.Fatal("symlink accepted")
	}
}

// TestCaseInsensitivePathCollisionsAreRejected protects worktrees on case-folding filesystems.
func TestCaseInsensitivePathCollisionsAreRejected(t *testing.T) {
	a := fileFor(t, acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first"))
	b := fileFor(t, acceptedIssue("local-ffeeddccbbaa99887766554433221100", "session/1/second"))
	a.Path, b.Path = "docs/issues/Export.md", "docs/issues/export.md"
	if _, err := localissues.Validate([]localissues.File{a, b}); err == nil {
		t.Fatal("case-fold filename collision accepted")
	}
}

// TestDirectoryPreflightRefusesANestedDirectory keeps docs/issues flat: a
// subdirectory is not an issue file, so the whole graph is refused.
func TestDirectoryPreflightRefusesANestedDirectory(t *testing.T) {
	root := t.TempDir()
	file := fileFor(t, acceptedIssue("local-00112233445566778899aabbccddeeff", "session/1/first"))
	if err := os.MkdirAll(filepath.Join(root, "docs", "issues", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file.Path)), file.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := localissues.ReadDirectory(root); err == nil {
		t.Fatal("nested directory accepted")
	}
}
