package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// scriptedGh answers gh calls by their first two arguments and records them.
type scriptedGh struct {
	answers map[string]string
	calls   [][]string
}

// Run records the call and returns the scripted answer.
func (g *scriptedGh) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	g.calls = append(g.calls, args)
	return []byte(g.answers[args[0]+" "+args[1]]), nil
}

// invokeBugReport runs the bug-report command and returns its exit code and output.
func invokeBugReport(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	code := Run(context.Background(), append([]string{"bug-report"}, args...), &output, &errorsOutput)
	return code, output.String(), errorsOutput.String()
}

// TestBugReportWritesAPrivateOfflineDraftWithoutConfiguration verifies that
// an installation without a usable configuration still gets a partial draft,
// with private permissions, without configuration content or local paths,
// and that an existing draft is replaced only with --force.
func TestBugReportWritesAPrivateOfflineDraftWithoutConfiguration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("token: ghp_privatetoken\nrepositories: [ {path: /Users/alice/acme-private}"), 0o600); err != nil {
		t.Fatal(err)
	}
	draftPath := filepath.Join(root, "report.md")

	code, output, errorsOutput := invokeBugReport(t, "--config", configPath, "--output", draftPath)
	if code != 0 {
		t.Fatalf("bug-report exit code = %d, stderr = %q", code, errorsOutput)
	}
	info, err := os.Stat(draftPath)
	if err != nil {
		t.Fatalf("draft was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("draft mode = %v, want 0600", info.Mode().Perm())
	}
	draft, _ := os.ReadFile(draftPath)
	for _, want := range []string{"factory-bug-report id=fbr-", "Host configuration: unavailable", "factory doctor"} {
		if !strings.Contains(string(draft), want) {
			t.Errorf("draft lacks %q:\n%s", want, draft)
		}
	}
	for _, private := range []string{"ghp_privatetoken", "acme-private", "/Users/alice", root} {
		if strings.Contains(string(draft), private) {
			t.Errorf("draft contains private value %q", private)
		}
	}
	if !strings.Contains(output, "factory bug-report submit") {
		t.Fatalf("stdout = %q, want the submission step", output)
	}

	if code, _, errorsOutput := invokeBugReport(t, "--config", configPath, "--output", draftPath); code != 1 || !strings.Contains(errorsOutput, "--force") {
		t.Fatalf("bug-report over an existing draft exit code = %d, stderr = %q; want a refusal that names --force", code, errorsOutput)
	}
	if unchanged, _ := os.ReadFile(draftPath); !bytes.Equal(unchanged, draft) {
		t.Fatal("bug-report changed the existing draft without --force")
	}
	if code, _, errorsOutput := invokeBugReport(t, "--config", configPath, "--output", draftPath, "--force"); code != 0 {
		t.Fatalf("bug-report --force exit code = %d, stderr = %q", code, errorsOutput)
	}
	if replaced, _ := os.ReadFile(draftPath); bytes.Equal(replaced, draft) {
		t.Fatal("bug-report --force did not replace the draft")
	}
}

// TestBugReportRequiresAnOutputPath verifies the usage error.
func TestBugReportRequiresAnOutputPath(t *testing.T) {
	if code, _, _ := invokeBugReport(t); code != 2 {
		t.Fatalf("bug-report exit code = %d, want 2", code)
	}
}

// TestBugReportSubmitTargetsTheFactoryRepositoryFromAnyDirectory verifies
// that submission from an unrelated checkout with another remote creates the
// issue in the Factory repository and prints the outcome and the URL.
func TestBugReportSubmitTargetsTheFactoryRepositoryFromAnyDirectory(t *testing.T) {
	root := t.TempDir()
	draftPath := filepath.Join(root, "report.md")
	if code, _, errorsOutput := invokeBugReport(t, "--config", filepath.Join(root, "missing.yaml"), "--output", draftPath); code != 0 {
		t.Fatalf("bug-report exit code = %d, stderr = %q", code, errorsOutput)
	}
	completeDraft(t, draftPath)
	unrelated := filepath.Join(root, "azure-checkout")
	if err := os.MkdirAll(filepath.Join(unrelated, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://dev.azure.com/acme/project/_git/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(unrelated)
	gh := &scriptedGh{answers: map[string]string{
		"issue list":   "[]",
		"label list":   `[{"name":"needs-triage"}]`,
		"issue create": "https://github.com/Stevie1704/sw-factory/issues/300\n",
	}}
	original := bugReportGhRunner
	t.Cleanup(func() { bugReportGhRunner = original })
	bugReportGhRunner = gh

	code, output, errorsOutput := invokeBugReport(t, "submit", draftPath)
	if code != 0 {
		t.Fatalf("bug-report submit exit code = %d, stderr = %q", code, errorsOutput)
	}
	if !strings.Contains(output, "outcome: created") || !strings.Contains(output, "https://github.com/Stevie1704/sw-factory/issues/300") {
		t.Fatalf("stdout = %q, want the created outcome and URL", output)
	}
	for _, call := range gh.calls {
		index := slices.Index(call, "--repo")
		if index < 0 || call[index+1] != "Stevie1704/sw-factory" {
			t.Fatalf("gh %v does not target Stevie1704/sw-factory", call)
		}
	}
}

// TestBugReportSubmitShowsDuplicateCandidates verifies the duplicate outcome,
// its exit code, and the documented way to submit a distinct defect.
func TestBugReportSubmitShowsDuplicateCandidates(t *testing.T) {
	root := t.TempDir()
	draftPath := filepath.Join(root, "report.md")
	if code, _, errorsOutput := invokeBugReport(t, "--config", filepath.Join(root, "missing.yaml"), "--output", draftPath); code != 0 {
		t.Fatalf("bug-report exit code = %d, stderr = %q", code, errorsOutput)
	}
	completeDraft(t, draftPath)
	original := bugReportGhRunner
	t.Cleanup(func() { bugReportGhRunner = original })
	bugReportGhRunner = &scriptedGh{answers: map[string]string{
		"issue list": `[{"number":42,"title":"Suspected Factory defect: status exits 1","url":"https://github.com/Stevie1704/sw-factory/issues/42","body":""}]`,
	}}

	code, output, _ := invokeBugReport(t, "submit", draftPath)
	if code != 3 || !strings.Contains(output, "outcome: duplicate_candidate") || !regexp.MustCompile(`#42 .*issues/42`).MatchString(output) || !strings.Contains(output, "--distinct-from") {
		t.Fatalf("bug-report submit exit code = %d, stdout = %q; want exit 3 with candidate #42 and the --distinct-from option", code, output)
	}
}

// completeDraft fills the title and the required sections of a draft file.
func completeDraft(t *testing.T, draftPath string) {
	t.Helper()
	draft, err := os.ReadFile(draftPath)
	if err != nil {
		t.Fatal(err)
	}
	completed := strings.Replace(string(draft), "<one-line summary>", "status exits 1", 1)
	for _, heading := range []string{"## Expected behavior\n\n", "## Observed behavior\n\n", "## Reproduction\n\n"} {
		completed = strings.Replace(completed, heading, heading+"Written by the operator.\n\n", 1)
	}
	if err := os.WriteFile(draftPath, []byte(completed), 0o600); err != nil {
		t.Fatal(err)
	}
}
