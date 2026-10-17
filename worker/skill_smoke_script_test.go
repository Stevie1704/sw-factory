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

// copilotSmokeToken is the dedicated Copilot credential the copilot smoke
// tests project. No assertion may find it outside the worker's standard input.
const copilotSmokeToken = "github_pat_smoke_test_only_0000"

// answeringCopilot is a stub Docker body for the copilot smoke. It logs every
// Docker argument list and the standard input of each prompt call, reports the
// pinned CLI version, and quotes the first instruction line of every skill.
// Each Docker call is one log line.
const answeringCopilot = `
printf '%s' "$*" | tr '\n' ' ' >> "$SMOKE_DOCKER_LOG"; echo >> "$SMOKE_DOCKER_LOG"
case "$*" in
  *--version*) printf 'GitHub Copilot CLI 1.0.98.\nRun '"'"'copilot update'"'"' to check for updates.\n' ;;
  *) cat >> "$SMOKE_STDIN_LOG"; awk 'FNR == 1 { markers = 0 } /^---$/ { markers++; next } markers >= 2 && NF { print; nextfile }' ../worker/skills/*/SKILL.md ;;
esac
`

// copilotSmokeRun is the observable result of one copilot smoke run: the
// script result, every Docker argument list, and the standard input the prompt
// calls received.
type copilotSmokeRun struct {
	skillSmokeRun
	dockerArguments string
	stdin           string
}

// runCopilotSmoke runs scripts/smoke-skills.sh for the copilot harness with a
// dedicated token file. The extra environment is applied last, so a test can
// replace the token path.
func runCopilotSmoke(t *testing.T, dockerBody string, environment ...string) copilotSmokeRun {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "copilot-token")
	if err := os.WriteFile(token, []byte(copilotSmokeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(dir, "docker.log")
	stdinLog := filepath.Join(dir, "stdin.log")
	base := []string{"HARNESSES=copilot", "COPILOT_TOKEN_PATH=" + token, "SMOKE_DOCKER_LOG=" + dockerLog, "SMOKE_STDIN_LOG=" + stdinLog}
	smoke := runSkillSmokeScript(t, "../scripts/smoke-skills.sh", dir, dockerBody, append(base, environment...)...)
	arguments, _ := os.ReadFile(dockerLog)
	stdin, _ := os.ReadFile(stdinLog)
	return copilotSmokeRun{skillSmokeRun: smoke, dockerArguments: string(arguments), stdin: string(stdin)}
}

// smokeRecord is one record of the skill smoke evidence file.
type smokeRecord struct {
	ImageDigest    string   `json:"image_digest"`
	Harness        string   `json:"harness"`
	HarnessVersion string   `json:"harness_version"`
	Skills         []string `json:"skills"`
}

// recordsOf returns the evidence records of one harness.
func recordsOf(t *testing.T, evidence, harness string) []smokeRecord {
	t.Helper()
	var recorded struct {
		Records []smokeRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(evidence), &recorded); err != nil {
		t.Fatalf("evidence is not JSON: %v\n%s", err, evidence)
	}
	var matching []smokeRecord
	for _, record := range recorded.Records {
		if record.Harness == harness {
			matching = append(matching, record)
		}
	}
	return matching
}

// TestSkillSmokeRecordsCopilotThroughTheLauncher verifies that the copilot
// smoke asks through the factory-copilot launcher, streams the dedicated
// token only through standard input, and keys the record by the image digest
// and the pinned CLI version line.
func TestSkillSmokeRecordsCopilotThroughTheLauncher(t *testing.T) {
	smoke := runCopilotSmoke(t, answeringCopilot)
	if smoke.err != nil {
		t.Fatalf("copilot smoke failed: %v\n%s", smoke.err, smoke.stderr)
	}
	records := recordsOf(t, smoke.evidence, "copilot")
	if len(records) != 1 {
		t.Fatalf("copilot records = %d, want 1:\n%s", len(records), smoke.evidence)
	}
	record := records[0]
	if record.HarnessVersion != "GitHub Copilot CLI 1.0.98." {
		t.Errorf("harness_version = %q, want the pinned CLI version line", record.HarnessVersion)
	}
	if record.ImageDigest != "sha256:0000000000000000000000000000000000000000000000000000000000000000" {
		t.Errorf("image_digest = %q", record.ImageDigest)
	}
	skills := slices.Clone(record.Skills)
	slices.Sort(skills)
	if got := strings.Join(skills, " "); got != "implement pr specification-review standards-review" {
		t.Errorf("recorded skills = %q", got)
	}
	if strings.Contains(smoke.dockerArguments, copilotSmokeToken) || strings.Contains(smoke.stderr, copilotSmokeToken) {
		t.Errorf("the token reached a Docker argument or the output:\n%s\n%s", smoke.dockerArguments, smoke.stderr)
	}
	if !strings.Contains(smoke.stdin, copilotSmokeToken) {
		t.Errorf("the token did not reach the worker through standard input")
	}
	for _, line := range strings.Split(strings.TrimSpace(smoke.dockerArguments), "\n") {
		if strings.Contains(line, "--version") {
			if !strings.Contains(line, "--network none") {
				t.Errorf("the version probe runs with network access: %s", line)
			}
			continue
		}
		for _, want := range []string{"factory-copilot", "--no-custom-instructions", "--disable-builtin-mcps", "--no-remote-export", "--secret-env-vars=COPILOT_GITHUB_TOKEN", "/run/factory-auth/copilot-token"} {
			if !strings.Contains(line, want) {
				t.Errorf("prompt call lacks %q: %s", want, line)
			}
		}
		for _, forbidden := range []string{"--allow-all ", "--yolo", "GH_TOKEN", "GITHUB_TOKEN="} {
			if strings.Contains(line, forbidden) {
				t.Errorf("prompt call contains %q: %s", forbidden, line)
			}
		}
	}
}

// TestSkillSmokeNeverRecordsCopilotWithoutADedicatedCredential verifies that
// an unset, empty, or missing dedicated token file skips the copilot smoke
// with a failing exit status and no record, even when the host offers a
// GitHub token or a Copilot home that a fallback could use.
func TestSkillSmokeNeverRecordsCopilotWithoutADedicatedCredential(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".copilot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".copilot", "config.json"), []byte(`{"token":"host"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tokenPath := range map[string]string{"unset": "", "empty file": empty, "missing file": empty + ".missing"} {
		t.Run(name, func(t *testing.T) {
			smoke := runCopilotSmoke(t, answeringCopilot,
				"COPILOT_TOKEN_PATH="+tokenPath, "HOME="+home, "GH_TOKEN=gho_host", "GITHUB_TOKEN=ghu_host")
			if smoke.err == nil {
				t.Fatalf("smoke passed without a dedicated Copilot credential:\n%s", smoke.stderr)
			}
			if records := recordsOf(t, smoke.evidence, "copilot"); len(records) != 0 {
				t.Errorf("smoke recorded copilot evidence without a credential:\n%s", smoke.evidence)
			}
			if smoke.dockerArguments != "" {
				t.Errorf("smoke started a worker without a credential:\n%s", smoke.dockerArguments)
			}
			if !strings.Contains(smoke.stderr, "No copilot credential") {
				t.Errorf("smoke does not name the missing credential:\n%s", smoke.stderr)
			}
		})
	}
}

// TestSkillSmokeNeverRecordsAFailedCopilotCall verifies that an
// authentication failure, a disabled organization policy, an unavailable
// model, or a reply without the skill content records no copilot evidence and
// fails the smoke. The token never reaches the smoke output.
func TestSkillSmokeNeverRecordsAFailedCopilotCall(t *testing.T) {
	tests := map[string]string{
		// The stderr line the pinned CLI printed offline for an unvalidated token.
		"token not validated": `echo "Error: Authentication token found but could not be validated." >&2; exit 1`,
		// The live shapes are unobserved (RB-5). Any non-zero exit must fail.
		"organization policy": `echo "Copilot CLI is disabled by your organization policy" >&2; exit 1`,
		"unavailable model":   `echo "Model is not available" >&2; exit 1`,
		// A zero exit without the skill content is not evidence either.
		"reply without skill": `echo "I cannot access that model right now."`,
	}
	for name, failure := range tests {
		t.Run(name, func(t *testing.T) {
			body := `
case "$*" in
  *--version*) echo "GitHub Copilot CLI 1.0.98." ;;
  *) cat > /dev/null; ` + failure + ` ;;
esac
`
			smoke := runCopilotSmoke(t, body)
			if smoke.err == nil {
				t.Fatalf("smoke passed with a failed Copilot call:\n%s", smoke.stderr)
			}
			if records := recordsOf(t, smoke.evidence, "copilot"); len(records) != 0 {
				t.Errorf("smoke recorded copilot evidence for a failed call:\n%s", smoke.evidence)
			}
			if strings.Contains(smoke.stderr, copilotSmokeToken) {
				t.Errorf("the token reached the smoke output")
			}
		})
	}
}
