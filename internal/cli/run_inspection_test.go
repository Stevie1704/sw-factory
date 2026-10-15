package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestRunsJSONPagesDeterministically verifies tied timestamps use run identity
// order and that a terminal historical run stays selectable beside an active run.
func TestRunsJSONPagesDeterministically(t *testing.T) {
	t.Parallel()
	configPath, dbPath, repository := registeredStatusFixture(t)
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"run-b", "run-a", "run-active"} {
		status := store.StatusComplete
		if id == "run-active" {
			status = store.StatusActive
		}
		if err := db.SaveRun(t.Context(), store.Run{ID: id, RepositoryPath: repository, Stage: store.StageImplementation, Status: status, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for offset, want := range []string{"run-a", "run-active", "run-b"} {
		var out, stderr bytes.Buffer
		args := []string{"runs", "--config", configPath, "--json", "--limit", "1", "--offset", strconv.Itoa(offset)}
		if code := cli.Run(t.Context(), args, &out, &stderr); code != 0 {
			t.Fatalf("runs exit %d: %s", code, stderr.String())
		}
		var doc struct {
			SchemaVersion int `json:"schema_version"`
			Result        struct {
				Runs []struct {
					ID string `json:"id"`
				} `json:"runs"`
				NextOffset *int `json:"next_offset"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.SchemaVersion != 1 || len(doc.Result.Runs) != 1 || doc.Result.Runs[0].ID != want {
			t.Fatalf("page %d: %s", offset, out.String())
		}
		if offset < 2 && (doc.Result.NextOffset == nil || *doc.Result.NextOffset != offset+1) {
			t.Fatalf("cursor: %s", out.String())
		}
		if offset == 2 && doc.Result.NextOffset != nil {
			t.Fatalf("last cursor: %s", out.String())
		}
	}
}

// TestRunInspectJSONSelectsHistoryWithoutLeakingContent verifies named
// inspection does not fall back to the active run or serialize private fields.
func TestRunInspectJSONSelectsHistoryWithoutLeakingContent(t *testing.T) {
	t.Parallel()
	configPath, dbPath, repository := registeredStatusFixture(t)
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const secret = "PRIVATE-credential-and-prompt"
	for _, run := range []store.Run{
		{ID: "old", Status: store.StatusComplete, SpecificationPacket: `{"version":1,"issue":{"body":"` + secret + `"},"repository_config":{"setup":"` + secret + `"}}`, LifecycleReason: secret, Worktree: "/private/" + secret, CheckRepairAttempts: 2, CheckRepairBudget: 3},
		{ID: "active", Status: store.StatusActive},
	} {
		run.RepositoryPath = repository
		run.Stage = store.StageImplementation
		run.CreatedAt = at
		run.UpdatedAt = at
		if err := db.SaveRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SavePendingEffect(t.Context(), store.PendingEffect{RunID: "old", ID: "effect-1", Kind: store.PendingEffectKindPush, Payload: `{"credential":"` + secret + `"}`}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveInvocation(t.Context(), store.Invocation{ID: "inv-old", RunID: "old", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusCompleted, NativeSessionID: secret, CredentialStoreID: secret, InvocationDirectory: secret, ResultDirectory: secret}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"run", "inspect", "--run-id", "old", "--json", "--config", configPath}, &out, &stderr); code != 0 {
		t.Fatalf("inspect exit %d: %s", code, stderr.String())
	}
	var doc struct {
		Result struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
			Policy struct {
				State  string `json:"state"`
				SHA256 string `json:"sha256"`
			} `json:"policy"`
			RetryBudgets map[string]struct {
				Used  int `json:"used"`
				Limit int `json:"limit"`
			} `json:"retry_budgets"`
			Coverage struct {
				Limitations []string `json:"limitations"`
			} `json:"coverage"`
		} `json:"result"`
	}
	decodeSingleDocument(t, out.String(), &doc)
	if doc.Result.Run.ID != "old" || doc.Result.Policy.State != "available" || len(doc.Result.Policy.SHA256) != 64 || doc.Result.RetryBudgets["check_repair"].Used != 2 || len(doc.Result.Coverage.Limitations) == 0 || strings.Contains(out.String(), secret) {
		t.Fatalf("inspection: %s", out.String())
	}
	after, err := db.Run(t.Context(), "old")
	if err != nil || after == nil || !after.UpdatedAt.Equal(at) || after.LifecycleReason != secret {
		t.Fatalf("inspection changed historical run: %+v %v", after, err)
	}
}

// TestRunEvidenceJSONReadsOnlyTheSelectedDiagnostic verifies the end-to-end
// selector handoff without relying on external executables.
func TestRunEvidenceJSONReadsOnlyTheSelectedDiagnostic(t *testing.T) {
	t.Parallel()
	configPath, dbPath, repository := registeredStatusFixture(t)
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	worktree := filepath.Join(t.TempDir(), "worktrees", "run-evidence")
	sha := strings.Repeat("a", 40)
	if err := db.SaveRun(t.Context(), store.Run{ID: "run-evidence", RepositoryPath: repository, Worktree: worktree, Stage: store.StageCheck, Status: store.StatusComplete, CheckpointSHA: sha}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(worktree), ".factory-agents", "run-evidence", "gate-failure", "checkpoint.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("observed at: now\nrun: run-evidence\nphase: checkpoint\ncheckpoint: "+sha+"\nfailure: test\n\nPRIVATE-COMMAND-OUTPUT"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"run", "inspect", "--run-id", "run-evidence", "--json", "--config", configPath}, &out, &stderr); code != 0 {
		t.Fatalf("inspect: %d %s", code, stderr.String())
	}
	var inspection struct {
		Result factory.RunInspection `json:"result"`
	}
	decodeSingleDocument(t, out.String(), &inspection)
	if strings.Contains(out.String(), "PRIVATE-COMMAND-OUTPUT") {
		t.Fatal("inspection read raw output")
	}
	var evidenceID string
	for _, ref := range inspection.Result.Evidence {
		if ref.Selector == "gate-failure.checkpoint" {
			evidenceID = ref.ID
		}
	}
	// Suggested actions must retain the selected installation, including for
	// absent artifacts, rather than silently falling back to the default store.
	for _, ref := range inspection.Result.Evidence {
		var selectedConfig string
		for i, arg := range ref.NextInspection {
			if arg == "--config" && i+1 < len(ref.NextInspection) {
				selectedConfig = ref.NextInspection[i+1]
			}
		}
		if selectedConfig != configPath {
			t.Fatalf("next inspection lost selected configuration: %v", ref.NextInspection)
		}
	}
	out.Reset()
	stderr.Reset()
	var nextInspection []string
	for _, ref := range inspection.Result.Evidence {
		if ref.ID == evidenceID {
			nextInspection = ref.NextInspection
		}
	}
	if code := cli.Run(t.Context(), nextInspection[1:], &out, &stderr); code != 0 {
		t.Fatalf("evidence: %d %s", code, stderr.String())
	}
	var page struct {
		Result factory.EvidencePage `json:"result"`
	}
	decodeSingleDocument(t, out.String(), &page)
	if !strings.Contains(page.Result.Content, "PRIVATE-COMMAND-OUTPUT") || page.Result.Evidence.ID != evidenceID {
		t.Fatalf("page: %s", out.String())
	}
}

// TestRunInspectionErrorsStayStructuredAndContentFree verifies stable unknown
// identities and unavailable stores while keeping usage errors on stderr.
func TestRunInspectionErrorsStayStructuredAndContentFree(t *testing.T) {
	t.Parallel()
	configPath, dbPath, repository := registeredStatusFixture(t)
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRun(t.Context(), store.Run{ID: "known", RepositoryPath: repository, Stage: store.StageCheck, Status: store.StatusComplete}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"run", "inspect", "--run-id", "missing"}, "run.not_found"},
		{[]string{"run", "evidence", "--run-id", "missing", "--evidence-id", "none"}, "run.not_found"},
		{[]string{"run", "evidence", "--run-id", "known", "--evidence-id", "../../host-secret"}, "evidence.not_found"},
	} {
		var out, stderr bytes.Buffer
		args := append(append([]string{}, tc.args...), "--json", "--config", configPath)
		if code := cli.Run(t.Context(), args, &out, &stderr); code != 1 || stderr.Len() != 0 {
			t.Fatalf("%v: exit %d %s", args, code, stderr.String())
		}
		var doc struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decodeSingleDocument(t, out.String(), &doc)
		if doc.Error.Code != tc.code || strings.Contains(out.String(), "host-secret") {
			t.Fatalf("%v: %s", args, out.String())
		}
	}
	for _, args := range [][]string{
		{"runs", "--limit", "101"}, {"runs", "--offset", "-1"},
		{"run", "inspect"}, {"run", "inspect", "--run-id", "known", "--limit", "0"},
		{"run", "evidence", "--run-id", "known", "--evidence-id", "none", "--limit", "65537"},
	} {
		var out, stderr bytes.Buffer
		args = append(args, "--json", "--config", configPath)
		if code := cli.Run(t.Context(), args, &out, &stderr); code != 2 || out.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("usage %v: exit %d out %q err %q", args, code, out.String(), stderr.String())
		}
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"runs"}, {"run", "inspect", "--run-id", "known"}, {"run", "evidence", "--run-id", "known", "--evidence-id", "none"}} {
		var out, stderr bytes.Buffer
		args = append(args, "--json", "--config", configPath)
		if code := cli.Run(t.Context(), args, &out, &stderr); code != 1 || !strings.Contains(out.String(), `"code":"inspection.unavailable"`) {
			t.Fatalf("missing store %v: %d %s %s", args, code, out.String(), stderr.String())
		}
		if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
			t.Fatalf("inspection recreated store: %v", err)
		}
	}
}
