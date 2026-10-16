package worker_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSkillSmokeReportsHarnessFailureSeparately verifies that a harness which
// fails before it answers, such as one with an expired credential, is
// reported with its own diagnosis instead of as a skill that did not load,
// and that no evidence is recorded for it.
func TestSkillSmokeReportsHarnessFailureSeparately(t *testing.T) {
	smoke := runSkillSmoke(t, `
case "$*" in
  *--version*) echo "2.1.232 (Claude Code)" ;;
  *) cat > /dev/null; echo "Failed to authenticate: OAuth session expired and could not be refreshed" >&2; exit 1 ;;
esac
`)
	if smoke.err == nil {
		t.Fatalf("smoke passed with a failing harness:\n%s", smoke.stderr)
	}
	if !strings.Contains(smoke.stderr, "OAuth session expired") {
		t.Errorf("smoke hides the harness diagnosis:\n%s", smoke.stderr)
	}
	if strings.Contains(smoke.stderr, "could not load") {
		t.Errorf("smoke reports a harness failure as a missing skill:\n%s", smoke.stderr)
	}
	if strings.Contains(smoke.evidence, `"harness": "claude"`) {
		t.Errorf("smoke recorded evidence for a failing harness:\n%s", smoke.evidence)
	}
}

// TestSkillSmokeRecordsAHarnessThatQuotesEachSkill verifies that a harness
// which answers with each skill's first instruction line is recorded.
func TestSkillSmokeRecordsAHarnessThatQuotesEachSkill(t *testing.T) {
	smoke := runSkillSmoke(t, `
case "$*" in
  *--version*) echo "2.1.232 (Claude Code)" ;;
  *) cat > /dev/null; echo "Smoke reply"; echo "harness warning" >&2; awk 'FNR == 1 { markers = 0 } /^---$/ { markers++; next } markers >= 2 && NF { print; nextfile }' ../worker/skills/*/SKILL.md ;;
esac
`)
	if smoke.err != nil {
		t.Fatalf("smoke failed with an answering harness: %v\n%s", smoke.err, smoke.stderr)
	}
	if !strings.Contains(smoke.evidence, `"harness": "claude"`) {
		t.Errorf("smoke recorded no claude evidence:\n%s", smoke.evidence)
	}
}

// TestSkillSmokeShowsAHarnessFailureWrittenToStandardOutput verifies that the
// diagnosis of a harness which reports its failure on standard output, as
// Claude Code does for an expired credential, still reaches the operator.
func TestSkillSmokeShowsAHarnessFailureWrittenToStandardOutput(t *testing.T) {
	smoke := runSkillSmoke(t, `
case "$*" in
  *--version*) echo "2.1.232 (Claude Code)" ;;
  *) cat > /dev/null; echo "OAuth token has expired. Please run /login."; exit 1 ;;
esac
`)
	if smoke.err == nil || !strings.Contains(smoke.stderr, "OAuth token has expired") {
		t.Errorf("smoke hides a diagnosis on standard output: %v\n%s", smoke.err, smoke.stderr)
	}
}

// answeringHarness is a stub Docker body whose harness quotes the first
// instruction line of every skill, except for a prompt that names the skill
// in its first argument, which it answers with an unrelated reply.
const answeringHarness = `
case "$*" in
  *--version*) echo "2.1.232 (Claude Code)" ;;
  *"\` + "`" + `${FAILING_SKILL:-none}\` + "`" + `"*) cat > /dev/null; echo "I could not find a skill with that name." ;;
  *) cat > /dev/null; awk 'FNR == 1 { markers = 0 } /^---$/ { markers++; next } markers >= 2 && NF { print; nextfile }' ../worker/skills/*/SKILL.md ;;
esac
`

// TestSkillSmokeRecordsTheSkillsThatLoadedAndShowsTheFailedReply verifies that
// one skill that does not load neither discards the skills that did, nor hides
// what the harness answered instead.
func TestSkillSmokeRecordsTheSkillsThatLoadedAndShowsTheFailedReply(t *testing.T) {
	smoke := runSkillSmoke(t, answeringHarness, "FAILING_SKILL=pr")
	if smoke.err == nil {
		t.Fatalf("smoke passed with a skill that did not load:\n%s", smoke.stderr)
	}
	if !strings.Contains(smoke.stderr, "I could not find a skill with that name.") {
		t.Errorf("smoke hides the reply of the skill that did not load:\n%s", smoke.stderr)
	}
	if got := recordedSkills(t, smoke.evidence); got != "implement specification-review standards-review" {
		t.Errorf("recorded skills = %q, want the three that loaded", got)
	}
}

// TestSkillSmokeAddsARetriedSkillToTheRecord verifies that a retry of only the
// skill that failed completes the record of the same digest and harness
// version, so a retry needs one paid call instead of one per skill.
func TestSkillSmokeAddsARetriedSkillToTheRecord(t *testing.T) {
	dir := t.TempDir()
	runSkillSmokeIn(t, dir, answeringHarness, "FAILING_SKILL=pr")
	smoke := runSkillSmokeIn(t, dir, answeringHarness, "MANDATORY_SKILLS=pr")
	if smoke.err != nil {
		t.Fatalf("retry of one skill failed: %v\n%s", smoke.err, smoke.stderr)
	}
	if got := recordedSkills(t, smoke.evidence); got != "implement pr specification-review standards-review" {
		t.Errorf("recorded skills = %q, want all four", got)
	}
}

// TestSkillSmokeRequiresGrillingOnlyWhereAnIdeationRoleIsDeclared verifies
// that the default skill list follows harness.RequiredSkills: the pinned
// worker of a repository without an ideation role is not smoked for
// grilling, and the harness its factory.yaml selects for the ideation role
// is.
func TestSkillSmokeRequiresGrillingOnlyWhereAnIdeationRoleIsDeclared(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		harness string
		want    string
	}{
		{name: "no ideation role", policy: "role_harness_defaults:\n  implementation: claude\nallowed_overrides: []\n", harness: "claude",
			want: "implement pr specification-review standards-review"},
		{name: "ideation on this harness", policy: "role_harness_defaults:\n  implementation: codex\n  ideation: claude\nallowed_overrides: []\n", harness: "claude",
			want: "grilling implement pr specification-review standards-review"},
		{name: "ideation on another harness", policy: "role_harness_defaults:\n  ideation: codex\nallowed_overrides: []\n", harness: "claude",
			want: "implement pr specification-review standards-review"},
		{name: "harness override allowed", policy: "role_harness_defaults:\n  ideation: codex\nallowed_overrides: [model, harness]\n", harness: "claude",
			want: "grilling implement pr specification-review standards-review"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := repositoryCopyForSmoke(t, test.policy)
			smoke := runSkillSmokeScript(t, filepath.Join(root, "scripts", "smoke-skills.sh"), t.TempDir(), answeringHarness)
			if smoke.err != nil {
				t.Fatalf("smoke failed: %v\n%s", smoke.err, smoke.stderr)
			}
			if got := recordedSkills(t, smoke.evidence); got != test.want {
				t.Errorf("recorded skills = %q, want %q", got, test.want)
			}
		})
	}
}

// repositoryCopyForSmoke returns a repository root with the smoke script,
// the worker skills, and a factory.yaml that holds policy.
func repositoryCopyForSmoke(t *testing.T, policy string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile("../scripts/smoke-skills.sh")
	if err != nil {
		t.Fatal(err)
	}
	skills, err := filepath.Abs("skills")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "smoke-skills.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(skills, filepath.Join(root, "worker", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "factory.yaml"), []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// recordedSkills returns the sorted skills of the single recorded claude
// smoke result, separated by spaces.
func recordedSkills(t *testing.T, evidence string) string {
	t.Helper()
	var recorded struct {
		Records []struct {
			Harness string   `json:"harness"`
			Skills  []string `json:"skills"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(evidence), &recorded); err != nil {
		t.Fatalf("evidence is not JSON: %v\n%s", err, evidence)
	}
	if len(recorded.Records) != 1 || recorded.Records[0].Harness != "claude" {
		t.Fatalf("evidence has no single claude record:\n%s", evidence)
	}
	skills := slices.Clone(recorded.Records[0].Skills)
	slices.Sort(skills)
	return strings.Join(skills, " ")
}

// skillSmokeRun holds the observable result of one scripts/smoke-skills.sh run.
type skillSmokeRun struct {
	err      error
	stderr   string
	evidence string
}

// runSkillSmoke runs scripts/smoke-skills.sh for the claude harness against a
// stub Docker executable with the given shell body and extra environment, and
// returns its result.
func runSkillSmoke(t *testing.T, dockerBody string, environment ...string) skillSmokeRun {
	t.Helper()
	return runSkillSmokeIn(t, t.TempDir(), dockerBody, environment...)
}

// runSkillSmokeIn is runSkillSmoke with the evidence file kept in dir, so
// consecutive runs share one record.
func runSkillSmokeIn(t *testing.T, dir, dockerBody string, environment ...string) skillSmokeRun {
	t.Helper()
	return runSkillSmokeScript(t, "../scripts/smoke-skills.sh", dir, dockerBody, environment...)
}

// runSkillSmokeScript is runSkillSmokeIn for the smoke script at script.
func runSkillSmokeScript(t *testing.T, script, dir, dockerBody string, environment ...string) skillSmokeRun {
	t.Helper()
	docker := filepath.Join(dir, "docker")
	credential := filepath.Join(dir, "credentials.json")
	evidence := filepath.Join(dir, "skill-smoke.json")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\n"+dockerBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credential, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(script)
	command.Env = append(os.Environ(),
		"DOCKER="+docker,
		"HARNESSES=claude",
		"CLAUDE_AUTH_PATH="+credential,
		"EVIDENCE_FILE="+evidence,
		"WORKER_IMAGE=example.test/worker",
		"WORKER_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	command.Env = append(command.Env, environment...)
	var stderr strings.Builder
	command.Stderr = &stderr
	err := command.Run()
	recorded, readErr := os.ReadFile(evidence)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return skillSmokeRun{err: err, stderr: stderr.String(), evidence: string(recorded)}
}
