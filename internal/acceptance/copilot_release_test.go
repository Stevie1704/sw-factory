package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// copilotLiveScenario is the acceptance scenario whose live result releases
// the Copilot harness.
const copilotLiveScenario = "copilot-mixed-harness-recovery"

// TestCommittedCopilotSmokeEvidenceRequiresARecordedLiveRun keeps the
// committed evidence consistent with the release status: a Copilot skill
// smoke record may exist only for the proved CLI release, and only for the
// worker digest of a passed live run in the Copilot release results. A
// skipped or offline probe therefore can never reach the evidence that
// startup diagnosis reads.
func TestCommittedCopilotSmokeEvidenceRequiresARecordedLiveRun(t *testing.T) {
	liveDigest := copilotLivePassDigest(t)
	evidence, err := harness.LoadSkillSmokeEvidence(filepath.Join(repositoryRoot, "worker", "skill-smoke.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range evidence.Records {
		if record.Harness != harness.NameCopilot {
			continue
		}
		if liveDigest == "" || record.ImageDigest != liveDigest {
			t.Errorf("worker/skill-smoke.json records copilot for %s, but docs/acceptance/copilot-results.json has no passed live %s run for that digest", record.ImageDigest, copilotLiveScenario)
		}
		if record.HarnessVersion != worker.CopilotVersionLine {
			t.Errorf("copilot record for %s has version %q, want the proved %q", record.ImageDigest, record.HarnessVersion, worker.CopilotVersionLine)
		}
	}
}

// copilotLivePassDigest returns the worker digest of the Copilot release
// results when they record a passed live run of the release scenario, and
// an empty string otherwise.
func copilotLivePassDigest(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repositoryRoot, "docs", "acceptance", "copilot-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results struct {
		WorkerDigest string `json:"worker_digest"`
		Results      []struct {
			Scenario string `json:"scenario"`
			Evidence string `json:"evidence"`
			Outcome  string `json:"outcome"`
		} `json:"results"`
	}
	if err := json.Unmarshal(content, &results); err != nil {
		t.Fatalf("copilot-results.json: %v", err)
	}
	for _, result := range results.Results {
		if result.Scenario == copilotLiveScenario && result.Evidence == "live" && result.Outcome == "pass" {
			return results.WorkerDigest
		}
	}
	return ""
}
