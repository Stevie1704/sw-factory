package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// operationDocument is the consumer view of a mutation command's --json
// result.
type operationDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Outcome       string `json:"outcome"`
	Attempted     bool   `json:"attempted"`
	Reason        *struct {
		Code string `json:"code"`
	} `json:"reason"`
	Identities struct {
		RunID string `json:"run_id"`
	} `json:"identities"`
	Verification *struct {
		Argv []string `json:"argv"`
	} `json:"verification"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// runOperationJSON runs one command and decodes its one JSON document.
func runOperationJSON(t *testing.T, args ...string) (operationDocument, int, string) {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), args, &output, &errorsOutput)
	var document operationDocument
	decodeSingleDocument(t, output.String(), &document)
	return document, code, errorsOutput.String()
}

// savePausedCheck persists one check run paused for check infrastructure.
func savePausedCheck(t *testing.T, operationalPath, repositoryPath string, status store.Status) *store.Store {
	t.Helper()
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if err := opened.SaveRun(context.Background(), store.Run{
		ID: "run-json", RepositoryPath: repositoryPath, IssueNumber: 21, Revision: 4,
		Stage: store.StageCheck, Status: status, CheckpointSHA: "0123456789abcdef0123456789abcdef01234567",
		LifecycleReason: "check infrastructure unavailable: worker container is not running", CreatedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	return opened
}

// TestRunResumeJSONRefusesAStaleOrMismatchedTarget verifies each guard
// returns one structured refusal on stdout, the text exit status, and no
// state change.
func TestRunResumeJSONRefusesAStaleOrMismatchedTarget(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		status store.Status
		flags  []string
		code   string
	}{
		"wrong run":          {store.StatusWaitingForHuman, []string{"--run-id", "run-other"}, "run.mismatch"},
		"stale revision":     {store.StatusWaitingForHuman, []string{"--run-id", "run-json", "--expect-revision", "3"}, "run.revision_changed"},
		"changed checkpoint": {store.StatusWaitingForHuman, []string{"--run-id", "run-json", "--expect-checkpoint", "fedcba9876543210fedcba9876543210fedcba98"}, "run.checkpoint_changed"},
		"terminal run":       {store.StatusCancelled, []string{"--run-id", "run-json"}, "run.terminal"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
			opened := savePausedCheck(t, operationalPath, repositoryPath, test.status)

			document, code, stderr := runOperationJSON(t, append([]string{"resume", "--json", "--config", configPath}, test.flags...)...)

			if code != 1 || stderr != "" {
				t.Fatalf("resume --json exit = %d stderr = %q, want 1 and no stderr", code, stderr)
			}
			if document.SchemaVersion != 1 || document.Command != "resume" || document.Outcome != "refused" || document.Attempted || document.Reason == nil || document.Reason.Code != test.code {
				t.Fatalf("document = %+v, want refusal %s", document, test.code)
			}
			if document.Verification == nil || strings.Join(document.Verification.Argv, " ") != "factory status --json" {
				t.Fatalf("verification = %+v, want factory status --json", document.Verification)
			}
			after, err := opened.Run(context.Background(), "run-json")
			if err != nil || after.Revision != 4 || after.Status != test.status {
				t.Fatalf("run after refusal = %+v (%v), want it unchanged", after, err)
			}
		})
	}
}

// TestRunResumeTextKeepsItsRefusalWording verifies the text view of the same
// refusal keeps its message and exit status.
func TestRunResumeTextKeepsItsRefusalWording(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	savePausedCheck(t, operationalPath, repositoryPath, store.StatusWaitingForHuman)
	var output, errorsOutput bytes.Buffer

	code := cli.Run(context.Background(), []string{"resume", "--config", configPath, "--run-id", "run-other"}, &output, &errorsOutput)

	if code != 1 || output.Len() != 0 || errorsOutput.String() != "error: active run is run-json, not run-other\n" {
		t.Fatalf("exit = %d stdout = %q stderr = %q, want the text refusal", code, output.String(), errorsOutput.String())
	}
}

// TestRunReconcileJSONRefusesAChangedEffect verifies abandoning an effect
// that is no longer pending is refused and keeps the pending effect.
func TestRunReconcileJSONRefusesAChangedEffect(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	opened := savePausedCheck(t, operationalPath, repositoryPath, store.StatusWaitingForHuman)
	savePendingEffect(t, opened, "effect-current")

	document, code, _ := runOperationJSON(t, "reconcile", "--json", "--config", configPath, "--run-id", "run-json", "--abandon-effect", "effect-reviewed", "--reason", "inspected")

	if code != 1 || document.Command != "reconcile" || document.Outcome != "refused" || document.Reason == nil || document.Reason.Code != "pending_effect.changed" {
		t.Fatalf("exit = %d document = %+v, want the changed-effect refusal", code, document)
	}
	pending, err := opened.PendingEffect(context.Background(), "run-json")
	if err != nil || pending == nil || pending.ID != "effect-current" {
		t.Fatalf("pending effect = %+v (%v), want it kept", pending, err)
	}
}

// TestRunStatusJSONNeverAdvertisesAbandonmentAsExecutable verifies the
// human-reviewed abandonment is a human decision whose reason is human input.
func TestRunStatusJSONNeverAdvertisesAbandonmentAsExecutable(t *testing.T) {
	t.Parallel()
	configPath, operationalPath, repositoryPath := registeredStatusFixture(t)
	opened := savePausedCheck(t, operationalPath, repositoryPath, store.StatusWaitingForHuman)
	savePendingEffect(t, opened, "effect-current")
	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--json", "--config", configPath}, &output, &errorsOutput); code != 0 {
		t.Fatalf("status exit = %d stderr = %q", code, errorsOutput.String())
	}
	var document struct {
		Recovery struct {
			Actions []struct {
				ID            string   `json:"id"`
				Kind          string   `json:"kind"`
				Authorization string   `json:"authorization"`
				Argv          []string `json:"argv"`
				Arguments     []struct {
					Flag       string `json:"flag"`
					Value      string `json:"value"`
					HumanInput bool   `json:"human_input"`
				} `json:"arguments"`
				Target struct {
					EffectID string `json:"effect_id"`
				} `json:"target"`
			} `json:"actions"`
		} `json:"recovery"`
	}
	decodeSingleDocument(t, output.String(), &document)
	found := false
	for _, action := range document.Recovery.Actions {
		if action.Kind == "executable" && action.ID == "pending_effect.abandon" {
			t.Fatalf("action = %+v, want abandonment never executable", action)
		}
		if action.ID != "pending_effect.abandon" {
			continue
		}
		found = true
		reasonIsHumanInput := false
		for _, argument := range action.Arguments {
			reasonIsHumanInput = reasonIsHumanInput || (argument.Flag == "reason" && argument.HumanInput && argument.Value == "")
		}
		if action.Kind != "human_decision" || action.Authorization != "human_review" || action.Argv != nil || !reasonIsHumanInput || action.Target.EffectID != "effect-current" {
			t.Fatalf("action = %+v, want a human-reviewed decision without a complete argv", action)
		}
	}
	if !found {
		t.Fatalf("actions = %+v, want pending_effect.abandon", document.Recovery.Actions)
	}
}

// TestRunResumeJSONReportsAnOperationalFailure verifies an unloadable
// configuration is one error document with the text exit status.
func TestRunResumeJSONReportsAnOperationalFailure(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("schema_version: [not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	document, code, stderr := runOperationJSON(t, "resume", "--json", "--config", configPath)

	if code != 1 || document.Outcome != "error" || document.Error == nil || document.Error.Code != "resume.operational_failure" || !document.Attempted {
		t.Fatalf("exit = %d document = %+v, want the operational error document", code, document)
	}
	if !strings.HasPrefix(stderr, "error: ") {
		t.Fatalf("stderr = %q, want the error detail", stderr)
	}
}

// TestRunCleanupJSONRequiresConfirmation verifies the unconfirmed cleanup is
// a structured refusal with the text exit status 2.
func TestRunCleanupJSONRequiresConfirmation(t *testing.T) {
	t.Parallel()
	configPath, _, _ := registeredStatusFixture(t)

	document, code, _ := runOperationJSON(t, "cleanup", "--json", "--config", configPath)

	if code != 2 || document.Outcome != "refused" || document.Reason == nil || document.Reason.Code != "cleanup.confirmation_required" || document.Attempted {
		t.Fatalf("exit = %d document = %+v, want the confirmation refusal", code, document)
	}
}

// TestRunOperationJSONKeepsUsageErrorsOnStderr verifies the documented
// exception for argument errors applies to every mutation command.
func TestRunOperationJSONKeepsUsageErrorsOnStderr(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"resume", "reconcile", "stop", "init", "register", "bootstrap-labels", "agent", "draft-pr", "poll", "cleanup"} {
		var output, errorsOutput bytes.Buffer
		code := cli.Run(context.Background(), []string{command, "--json", "extra"}, &output, &errorsOutput)
		if code != 2 || output.Len() != 0 || errorsOutput.Len() == 0 {
			t.Fatalf("%s: exit = %d stdout = %q stderr = %q, want 2 with stderr only", command, code, output.String(), errorsOutput.String())
		}
	}
}

// savePendingEffect reserves one status-comment effect for run-json.
func savePendingEffect(t *testing.T, opened *store.Store, effectID string) {
	t.Helper()
	now := time.Date(2026, 10, 14, 9, 5, 0, 0, time.UTC)
	if err := opened.SavePendingEffect(context.Background(), store.PendingEffect{
		RunID: "run-json", ID: effectID, Kind: store.PendingEffectKindStatusComment, Payload: `{}`, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}
