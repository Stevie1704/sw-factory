package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// copilotLiveScenario is the acceptance scenario whose live result releases
// the Copilot harness.
const copilotLiveScenario = "copilot-mixed-harness-recovery"

// TestCommittedCopilotSmokeEvidenceRequiresARecordedLiveRun keeps the
// committed evidence consistent with the release status: a Copilot skill
// smoke record may exist only for the proved CLI release, and only when the
// Copilot release results record a passed live run. A skipped or offline
// probe therefore can never reach the evidence that startup diagnosis reads.
func TestCommittedCopilotSmokeEvidenceRequiresARecordedLiveRun(t *testing.T) {
	livePassed := copilotLiveRunPassed(t)
	for _, record := range committedSmokeRecords(t) {
		if record.Harness != "copilot" {
			continue
		}
		if !livePassed {
			t.Errorf("worker/skill-smoke.json records copilot for %s, but docs/acceptance/copilot-results.json has no passed live %s run", record.ImageDigest, copilotLiveScenario)
		}
		if record.HarnessVersion != worker.CopilotVersionLine {
			t.Errorf("copilot record for %s has version %q, want the proved %q", record.ImageDigest, record.HarnessVersion, worker.CopilotVersionLine)
		}
	}
}

// committedSmokeRecord is one record of worker/skill-smoke.json.
type committedSmokeRecord struct {
	ImageDigest    string `json:"image_digest"`
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version"`
}

// committedSmokeRecords reads the committed skill smoke evidence.
func committedSmokeRecords(t *testing.T) []committedSmokeRecord {
	t.Helper()
	var evidence struct {
		Records []committedSmokeRecord `json:"records"`
	}
	readJSON(t, filepath.Join(repositoryRoot, "worker", "skill-smoke.json"), &evidence)
	return evidence.Records
}

// copilotLiveRunPassed reports whether the Copilot release results record a
// passed live run of the release scenario.
func copilotLiveRunPassed(t *testing.T) bool {
	t.Helper()
	var results struct {
		Results []struct {
			Scenario string `json:"scenario"`
			Evidence string `json:"evidence"`
			Outcome  string `json:"outcome"`
		} `json:"results"`
	}
	readJSON(t, filepath.Join(repositoryRoot, "docs", "acceptance", "copilot-results.json"), &results)
	for _, result := range results.Results {
		if result.Scenario == copilotLiveScenario && result.Evidence == "live" && result.Outcome == "pass" {
			return true
		}
	}
	return false
}

// readJSON decodes the JSON file at path into target.
func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, target); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
