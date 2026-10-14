package onboarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Load reads one plan without taking its lock. A reader sees either the
// previous or the next complete plan, because a write replaces the file
// atomically.
func Load(path string) (Plan, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, fmt.Errorf("read the onboarding plan %s: %w", path, err)
	}
	var plan Plan
	if err := json.Unmarshal(body, &plan); err != nil {
		return Plan{}, fmt.Errorf("the onboarding plan %s is not readable: %w", path, err)
	}
	if plan.SchemaVersion != PlanSchema {
		return Plan{}, fmt.Errorf("the onboarding plan %s has schema_version %d; this build reads %d", path, plan.SchemaVersion, PlanSchema)
	}
	if plan.Evidence == nil {
		plan.Evidence = map[Step]Evidence{}
	}
	return plan, nil
}

// Update applies change to the plan at path under an exclusive lock and
// writes the result atomically with the next revision. The lock serializes
// writers, so a change always starts from the latest plan and never
// overwrites another writer's progress. A change that returns an error
// writes nothing. For a missing plan, change receives an empty plan and
// exists is false.
func Update(path string, change func(plan *Plan, exists bool) error) (Plan, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Plan{}, fmt.Errorf("create the onboarding plan directory: %w", err)
	}
	unlock, err := lock(path + ".lock")
	if err != nil {
		return Plan{}, err
	}
	defer unlock()
	plan, err := Load(path)
	exists := err == nil
	if errors.Is(err, fs.ErrNotExist) {
		plan, err = Plan{}, nil
	}
	if err != nil {
		return Plan{}, err
	}
	if err := change(&plan, exists); err != nil {
		return Plan{}, err
	}
	plan.SchemaVersion = PlanSchema
	plan.Revision++
	if plan.Evidence == nil {
		plan.Evidence = map[Step]Evidence{}
	}
	if err := writeAtomically(path, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// lock takes the exclusive lock file of one plan and returns its release.
// It waits for another writer to finish.
func lock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the onboarding plan lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock the onboarding plan: %w", err)
	}
	return func() { _ = file.Close() }, nil
}

// writeAtomically writes plan to a new file beside path and renames it into
// place, so an interrupted write leaves the previous plan.
func writeAtomically(path string, plan Plan) error {
	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".next-*")
	if err != nil {
		return fmt.Errorf("write the onboarding plan: %w", err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	_, err = temporary.Write(append(body, '\n'))
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write the onboarding plan: %w", err)
	}
	return nil
}
