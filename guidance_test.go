package swfactory_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// contributorGuidance are the files a fresh coding agent reads first to find
// its way to a module, its tests, and the verification command.
var contributorGuidance = []string{"AGENTS.md", "README.md", "docs/agents/domain.md", "docs/agents/issue-tracker.md", "docs/agents/triage-labels.md"}

// relativeLink matches the target of a Markdown link that is not a URL or an
// in-page anchor.
var relativeLink = regexp.MustCompile(`\]\(([^)#:]+)(?:#[^)]*)?\)`)

// rootDocument matches a root-level Markdown file named in code format, such
// as `GLOSSARY.md`.
var rootDocument = regexp.MustCompile("`([A-Z][A-Z_-]*\\.md)`")

// TestContributorGuidanceLinksResolve verifies that every file the
// contributor guidance links or names exists, and that the guidance names the
// repository verification command.
func TestContributorGuidanceLinksResolve(t *testing.T) {
	for _, name := range contributorGuidance {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range relativeLink.FindAllStringSubmatch(string(data), -1) {
			if _, err := os.Stat(filepath.Join(filepath.Dir(name), match[1])); err != nil {
				t.Errorf("%s links to missing %s", name, match[1])
			}
		}
		if name != "AGENTS.md" {
			continue
		}
		for _, match := range rootDocument.FindAllStringSubmatch(string(data), -1) {
			if _, err := os.Stat(match[1]); err != nil {
				t.Errorf("%s names missing %s", name, match[1])
			}
		}
		if !strings.Contains(string(data), "make check") {
			t.Errorf("%s does not name make check", name)
		}
	}
}
