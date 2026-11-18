package acceptance_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/acceptance"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/version"
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
	results, err := os.Open(filepath.Join(repositoryRoot, "docs", "acceptance", "copilot-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = results.Close() }()
	liveDigest, err := copilotLivePassDigest(results)
	if err != nil {
		t.Fatalf("docs/acceptance/copilot-results.json is refused by the report validation: %v", err)
	}
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

// releasedDigest is a valid worker digest for the results cases below.
const releasedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// copilotLivePass is a results file that the report command accepts and that
// records a passed live run of the Copilot release scenario. Each
// replacement in TestRefusedCopilotResultsNeverReleaseASmokeRecord makes it
// a file that the report command refuses.
const copilotLivePass = `{
  "schema_version": 1,
  "host_harness": {"name": "codex", "version": "codex-cli 0.148.0"},
  "worker_digest": "` + releasedDigest + `",
  "worker_harnesses": [],
  "results": [
    {"scenario": "copilot-mixed-harness-recovery", "evidence": "live", "outcome": "pass", "duration_seconds": 60, "usage": {"measured": false, "reason": "not measured"}}
  ]
}`

// TestRefusedCopilotResultsNeverReleaseASmokeRecord verifies that only a
// results file which passes the canonical decoder and the report validation
// can release a committed Copilot smoke record. A file that `factory
// acceptance report` refuses yields no release digest, even when it contains
// a passed live result.
func TestRefusedCopilotResultsNeverReleaseASmokeRecord(t *testing.T) {
	digest, err := copilotLivePassDigest(strings.NewReader(copilotLivePass))
	if err != nil || digest != releasedDigest {
		t.Fatalf("valid live pass gives digest %q, error %v; want %q", digest, err, releasedDigest)
	}
	refused := map[string][2]string{
		"unknown field":          {`"worker_harnesses": []`, `"worker_harnesses": [], "released": true`},
		"wrong schema version":   {`"schema_version": 1`, `"schema_version": 2`},
		"duplicate live result":  {`}}` + "\n  ]", `}},` + "\n" + `    {"scenario": "copilot-mixed-harness-recovery", "evidence": "live", "outcome": "fail", "reason": "contradicts the pass", "duration_seconds": 60, "usage": {"measured": false, "reason": "not measured"}}` + "\n  ]"},
		"live pass without host": {`"host_harness": {"name": "codex", "version": "codex-cli 0.148.0"}`, `"host_harness": null`},
		"unmeasured live pass":   {`, "duration_seconds": 60, "usage": {"measured": false, "reason": "not measured"}`, ``},
		"malformed digest":       {releasedDigest, "sha256:short"},
		"second document":        {"\n}", "\n}\n{}"},
	}
	for name, replacement := range refused {
		t.Run(name, func(t *testing.T) {
			source := strings.Replace(copilotLivePass, replacement[0], replacement[1], 1)
			if source == copilotLivePass {
				t.Fatalf("replacement %q did not change the results", replacement[0])
			}
			digest, err := copilotLivePassDigest(strings.NewReader(source))
			if err == nil || digest != "" {
				t.Errorf("refused results give digest %q, error %v; want no digest and an error", digest, err)
			}
		})
	}
}

// copilotLivePassDigest validates one results file as `factory acceptance
// report` does and returns its worker digest when it records a passed live
// run of the Copilot release scenario. It returns an empty digest when no
// such run exists, and an error when the report command would refuse the
// file.
func copilotLivePassDigest(source io.Reader) (string, error) {
	manifest, err := acceptance.LoadManifest()
	if err != nil {
		return "", err
	}
	results, err := acceptance.DecodeResults(source)
	if err != nil {
		return "", err
	}
	report, err := acceptance.BuildReport(manifest, results, version.Identity{}, time.Time{})
	if err != nil {
		return "", err
	}
	for _, scenario := range report.Scenarios {
		if scenario.ID == copilotLiveScenario && scenario.Live.Status == acceptance.OutcomePass {
			return report.WorkerDigest, nil
		}
	}
	return "", nil
}
