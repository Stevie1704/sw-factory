package worker_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

// skillSmokeRun holds the observable result of one scripts/smoke-skills.sh run.
type skillSmokeRun struct {
	err      error
	stderr   string
	evidence string
}

// runSkillSmoke runs scripts/smoke-skills.sh for the claude harness against a
// stub Docker executable with the given shell body, and returns its result.
func runSkillSmoke(t *testing.T, dockerBody string) skillSmokeRun {
	t.Helper()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	credential := filepath.Join(dir, "credentials.json")
	evidence := filepath.Join(dir, "skill-smoke.json")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\n"+dockerBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credential, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("../scripts/smoke-skills.sh")
	command.Env = append(os.Environ(),
		"DOCKER="+docker,
		"HARNESSES=claude",
		"CLAUDE_AUTH_PATH="+credential,
		"EVIDENCE_FILE="+evidence,
		"WORKER_IMAGE=example.test/worker",
		"WORKER_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	var stderr strings.Builder
	command.Stderr = &stderr
	err := command.Run()
	recorded, readErr := os.ReadFile(evidence)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return skillSmokeRun{err: err, stderr: stderr.String(), evidence: string(recorded)}
}
