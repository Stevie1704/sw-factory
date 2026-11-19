package store_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestFullQuiescenceNamesEachCondition verifies that full quiescence names
// one finding for each kind of work in progress, and none for an idle store.
func TestFullQuiescenceNamesEachCondition(t *testing.T) {
	t.Parallel()
	idle := quiescentStore(t, completedRun("run-1", 7))
	if findings := fullQuiescence(t, idle); len(findings) != 0 {
		t.Fatalf("idle store findings = %+v, want none", findings)
	}
	for code, statement := range map[string]string{
		"run.active":                    "UPDATE operational_runs SET status = 'active'",
		"effect.pending":                `INSERT INTO pending_effects (run_id, effect_id, kind, payload, created_at, updated_at) VALUES ('run-1', 'effect-1', 'status_comment', '{}', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"invocation.active":             `INSERT INTO invocations (id, run_id, harness, role, stage, status, created_at, updated_at) VALUES ('inv-1', 'run-1', 'codex', 'implementation', 'implementation', 'active', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"session_invocation.active":     `INSERT INTO ideation_invocations (session_id, id, role, trigger_id, session_revision, context_revision, harness, prompt_version, prompt_sha256, worker_id, status, created_at, updated_at) VALUES ('session-1', 'session-inv-1', 'ideation', 'turn-1', 1, 'context-1', 'codex', 'ideation-v1', 'digest', 'worker-1', 'active', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"session_invocation.unreleased": `INSERT INTO ideation_invocations (session_id, id, role, trigger_id, session_revision, context_revision, harness, prompt_version, prompt_sha256, worker_id, status, created_at, updated_at) VALUES ('session-1', 'session-inv-1', 'ideation', 'turn-1', 1, 'context-1', 'codex', 'ideation-v1', 'digest', 'worker-1', 'failed', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"session_command.queued":        `INSERT INTO ideation_commands (request_id, session_id, kind, expected_revision, payload, state, accepted_at) VALUES ('request-1', 'session-1', 'submit_turn', 1, '{}', 'accepted', '2026-11-01T00:00:00Z')`,
		"publication.in_progress":       `INSERT INTO ideation_sessions (id, repository_path, state, revision, created_at, updated_at) VALUES ('session-1', '/work/service', 'publication_incomplete', 1, '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
	} {
		path := quiescentStore(t, completedRun("run-1", 7))
		if _, err := openRaw(t, path).Exec(statement); err != nil {
			t.Fatalf("%s fixture: %v", code, err)
		}
		findings := fullQuiescence(t, path)
		codes := make([]string, 0, len(findings))
		for _, finding := range findings {
			codes = append(codes, finding.Code)
		}
		if !slices.Equal(codes, []string{code}) {
			t.Fatalf("%s findings = %+v, want exactly that condition", code, findings)
		}
	}
}

// fullQuiescence runs the full-quiescence check on a raw connection of the
// store at path.
func fullQuiescence(t *testing.T, path string) []store.UpgradeFinding {
	t.Helper()
	findings, err := store.FullQuiescenceFindings(context.Background(), openRaw(t, filepath.Clean(path)))
	if err != nil {
		t.Fatalf("FullQuiescenceFindings() error = %v", err)
	}
	return findings
}
