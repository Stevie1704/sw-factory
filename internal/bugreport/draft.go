package bugreport

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stevie1704/sw-factory/internal/version"
)

// TitlePlaceholder is the part of the draft title the operator replaces with
// a one-line summary.
const TitlePlaceholder = "<one-line summary>"

// titlePrefix starts every report title, so maintainers see the
// classification before the summary.
const titlePrefix = "Suspected Factory defect: "

// Limits keep a submission inside the upstream issue limits.
const (
	maxTitleRunes = 256
	maxBodyBytes  = 60000
)

// draftSection is one operator-written section of a draft.
type draftSection struct {
	heading  string
	hint     string
	required bool
}

// draftSections are the operator-written sections in draft order. A required
// section must contain text before submission.
var draftSections = []draftSection{
	{"Expected behavior", "required: what the guide or the command help says should happen; name the topic or the command", true},
	{"Observed behavior", "required: what happened, with the exact command, its exit status, and the error line", true},
	{"Reproduction", "required: the shortest command sequence that shows the behavior, or 'Not reproduced:' and why", true},
	{"Impact", "optional: what the defect stopped or changed", false},
	{"Why this appears to be a Factory defect", "optional: why the cause appears to be Factory, not the repository or the environment", false},
}

// markerPattern matches the identity marker, the first line of a draft and of
// the upstream issue body.
var markerPattern = regexp.MustCompile(`^<!-- factory-bug-report id=(fbr-[0-9a-f]{32}) schema=1 -->$`)

// commentPattern matches one single-line HTML comment, the form of every
// draft hint.
var commentPattern = regexp.MustCompile(`<!--.*?-->`)

// Draft is the content of one local suspected-defect report before the
// operator edits it.
type Draft struct {
	// ID is the stable report identity used to reconcile submission retries.
	ID string
	// Identity is the identity of the Factory build that prepared the draft.
	Identity version.Identity
	// Evidence is the allowlisted local evidence.
	Evidence Evidence
	// CreatedAt is when the draft was prepared.
	CreatedAt time.Time
}

// Submission is a validated draft, ready for upstream publication exactly as
// the operator reviewed it.
type Submission struct {
	// ID is the report identity from the draft marker.
	ID string
	// Title is the issue title from the draft title line.
	Title string
	// Summary is the operator's one-line summary: the title without its
	// classification prefix.
	Summary string
	// Body is the reviewed draft without its title line. It starts with the
	// identity marker, so the upstream issue carries the report identity.
	Body string
}

// Marker returns the identity marker line that starts a draft and the body
// of its upstream issue.
func Marker(reportID string) string {
	return "<!-- factory-bug-report id=" + reportID + " schema=1 -->"
}

// NewID returns a new random report identity.
func NewID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate report identity: %w", err)
	}
	return "fbr-" + hex.EncodeToString(random), nil
}

// Render writes a draft as Markdown. The operator completes the required
// sections and the title, then submits the file unchanged otherwise.
func Render(draft Draft) []byte {
	var out strings.Builder
	out.WriteString(Marker(draft.ID) + "\n")
	fmt.Fprintf(&out, "# %s%s\n\n", titlePrefix, TitlePlaceholder)
	out.WriteString("> Local draft prepared by `factory bug-report`. Factory sent nothing. Complete the title and the required sections, remove private content, then run `factory bug-report submit <file>`.\n\n")
	for _, section := range draftSections {
		fmt.Fprintf(&out, "## %s\n\n<!-- %s -->\n\n", section.heading, section.hint)
	}
	writeEvidence(&out, draft)
	return []byte(out.String())
}

// writeEvidence writes the collected evidence and the collection limits.
func writeEvidence(out *strings.Builder, draft Draft) {
	identity, evidence := draft.Identity, draft.Evidence
	out.WriteString("## Collected evidence\n\n")
	fmt.Fprintf(out, "- Report identity: %s\n", draft.ID)
	fmt.Fprintf(out, "- Prepared at: %s\n", draft.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "- Factory: %s (build=%s, revision=%s, modified=%t)\n", identity.Version, identity.Build, identity.Revision, identity.Modified)
	fmt.Fprintf(out, "- Platform: %s\n", identity.Platform)
	fmt.Fprintf(out, "- Worker base image: %s\n", valueOrUnavailable(identity.WorkerBaseImage))
	out.WriteString("- Harness versions: unavailable (Factory does not record harness versions)\n")
	fmt.Fprintf(out, "- Host configuration: %s\n", evidence.Configuration)
	fmt.Fprintf(out, "- Operational store: %s\n", evidence.Store)
	fmt.Fprintf(out, "- Provider: %s\n", valueOrUnavailable(evidence.Provider))
	if run := evidence.Run; run != nil {
		fmt.Fprintf(out, "\n### Run\n\n- Run: %s (stage=%s, status=%s)\n", run.ID, run.Stage, run.Status)
		for _, gate := range run.FailedGates {
			fmt.Fprintf(out, "- Failed blocking gate: phase=%s outcome=%s\n", gate.Phase, gate.Outcome)
		}
		for _, invocation := range run.Invocations {
			fmt.Fprintf(out, "- Invocation: %s (role=%s, stage=%s, harness=%s, status=%s, prompt=%s)\n", invocation.ID, invocation.Role, invocation.Stage, invocation.Harness, invocation.Status, valueOrUnavailable(invocation.PromptVersion))
		}
		for _, proposal := range run.DefectProposals {
			fmt.Fprintf(out, "- Worker defect proposal: Invocation %s proposed code %s\n", proposal.InvocationID, proposal.Code)
		}
	}
	out.WriteString("\n## Collection limitations\n\n")
	out.WriteString("- This is a suspected defect, not a confirmed root cause.\n")
	out.WriteString("- Automatic collection excludes credentials, prompts, transcripts, source, diffs, logs, repository identities, and local paths.\n")
	for _, limitation := range evidence.Limitations {
		fmt.Fprintf(out, "- %s\n", limitation)
	}
}

// valueOrUnavailable returns value, or an explicit unavailable marker.
func valueOrUnavailable(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unavailable"
	}
	return value
}

// Parse validates a reviewed draft and returns the exact submission. It
// rejects a draft without its identity marker, without a completed title, or
// without text in a required section.
func Parse(content []byte) (Submission, error) {
	if !utf8.Valid(content) {
		return Submission{}, errors.New("the draft is not valid UTF-8")
	}
	lines := strings.SplitN(string(content), "\n", 3)
	match := markerPattern.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if match == nil {
		return Submission{}, errors.New("the draft has no valid report identity marker on its first line; prepare a new draft with factory bug-report")
	}
	if len(lines) < 3 {
		return Submission{}, errors.New("the draft has no title and no sections")
	}
	var problems []string
	title := strings.TrimSpace(strings.TrimPrefix(lines[1], "# "))
	summary := strings.TrimSpace(strings.TrimPrefix(title, strings.TrimSpace(titlePrefix)))
	switch {
	case !strings.HasPrefix(lines[1], "# "+titlePrefix):
		problems = append(problems, fmt.Sprintf("the title line must start with %q", "# "+titlePrefix))
	case summary == "" || strings.Contains(title, TitlePlaceholder):
		problems = append(problems, "the title still has its placeholder")
	case utf8.RuneCountInString(title) > maxTitleRunes:
		problems = append(problems, fmt.Sprintf("the title exceeds %d characters", maxTitleRunes))
	}
	sections := sectionTexts(lines[2])
	for _, section := range draftSections {
		if section.required && strings.TrimSpace(commentPattern.ReplaceAllString(sections[section.heading], "")) == "" {
			problems = append(problems, fmt.Sprintf("section %q is empty", section.heading))
		}
	}
	body := lines[0] + "\n" + lines[2]
	if len(body) > maxBodyBytes {
		problems = append(problems, fmt.Sprintf("the body exceeds %d bytes", maxBodyBytes))
	}
	if len(problems) > 0 {
		return Submission{}, fmt.Errorf("the draft is not ready for submission: %s", strings.Join(problems, "; "))
	}
	return Submission{ID: match[1], Title: title, Summary: summary, Body: body}, nil
}

// sectionTexts maps each "## " heading to the text below it.
func sectionTexts(markdown string) map[string]string {
	sections := map[string]string{}
	current := ""
	for _, line := range strings.Split(markdown, "\n") {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			current = strings.TrimSpace(heading)
			continue
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}
