package onboarding

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestUpdateCreatesAndRevisesThePlan verifies that the first update creates
// a private plan file and that each update increases the revision.
func TestUpdateCreatesAndRevisesThePlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plans", "onboarding.json")
	created, err := Update(path, func(plan *Plan, exists bool) error {
		if exists {
			t.Fatal("a missing plan was reported as existing")
		}
		*plan = NewPlan("/repo", "/host/config.yaml", "")
		return nil
	})
	if err != nil || created.Revision != 1 {
		t.Fatalf("create = %+v, %v; want revision 1", created, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plan file = %v, %v; want mode 0600", info, err)
	}
	revised, err := Update(path, func(plan *Plan, exists bool) error {
		plan.Record(StepDoctor, Evidence{Outcome: OutcomePassed})
		return nil
	})
	if err != nil || revised.Revision != 2 {
		t.Fatalf("revise = %+v, %v; want revision 2", revised, err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Revision != 2 || loaded.Evidence[StepDoctor].Outcome != OutcomePassed || loaded.Repository != "/repo" {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
}

// TestUpdateKeepsThePlanWhenTheChangeFails verifies that a refused change
// writes nothing.
func TestUpdateKeepsThePlanWhenTheChangeFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	if _, err := Update(path, func(plan *Plan, _ bool) error {
		*plan = NewPlan("/repo", "/host/config.yaml", "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("refused")
	if _, err := Update(path, func(plan *Plan, _ bool) error {
		plan.Repository = "/other"
		return refusal
	}); !errors.Is(err, refusal) {
		t.Fatalf("update error = %v, want the refusal", err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Revision != 1 || loaded.Repository != "/repo" {
		t.Fatalf("load = %+v, %v; want the unchanged plan", loaded, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "onboarding.json" && entry.Name() != "onboarding.json.lock" {
			t.Fatalf("unexpected leftover %s", entry.Name())
		}
	}
}

// TestConcurrentUpdatesKeepEveryWritersProgress verifies that writers are
// serialized: no writer overwrites another writer's evidence.
func TestConcurrentUpdatesKeepEveryWritersProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	if _, err := Update(path, func(plan *Plan, _ bool) error {
		*plan = NewPlan("/repo", "/host/config.yaml", "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const writers = 16
	var group sync.WaitGroup
	for writer := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := Update(path, func(plan *Plan, _ bool) error {
				plan.Record(Step(fmt.Sprintf("writer-%d", writer)), Evidence{Outcome: OutcomePassed})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != writers+1 || len(loaded.Evidence) != writers {
		t.Fatalf("revision %d with %d evidence records, want %d and %d", loaded.Revision, len(loaded.Evidence), writers+1, writers)
	}
}

// TestLoadRefusesAnUnreadablePlan verifies that a corrupt or newer plan is
// refused instead of being treated as empty.
func TestLoadRefusesAnUnreadablePlan(t *testing.T) {
	directory := t.TempDir()
	for name, body := range map[string]string{
		"corrupt.json": "{",
		"newer.json":   `{"schema_version": 99, "evidence": {}}`,
	} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%s loaded without an error", name)
		}
		if _, err := Update(path, func(*Plan, bool) error { return nil }); err == nil {
			t.Fatalf("%s was updated without an error", name)
		}
	}
}
