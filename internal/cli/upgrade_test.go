package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// fakeUpgradeImages reports local images and their release label without
// Docker. It has no pull method, so a plan cannot pull.
type fakeUpgradeImages struct {
	release string
	missing map[string]bool
}

// CheckImage reports every image as present unless it is marked missing.
func (f fakeUpgradeImages) CheckImage(_ context.Context, image worker.ImageReference) error {
	if f.missing[image.Name] {
		return errors.New("no such image")
	}
	return nil
}

// ImageRelease returns the configured release label.
func (f fakeUpgradeImages) ImageRelease(context.Context, worker.ImageReference) (string, error) {
	return f.release, nil
}

// upgradeFixture is a registered repository with a current store.
type upgradeFixture struct {
	planFixture
	store string
}

// newUpgradeFixture registers the onboarding fixture, gives it a current
// store with one run of the given status, and makes the target a release
// binary that PATH does not name yet.
func newUpgradeFixture(t *testing.T, status store.Status, images fakeUpgradeImages) upgradeFixture {
	t.Helper()
	usePlanFakes(t)
	fixture := upgradeFixture{planFixture: newPlanFixture(t)}
	host, err := config.LoadHost(fixture.hostConfig)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = host.Repositories[0].OperationalDataPath
	opened, err := store.Open(context.Background(), fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: "run-1", RepositoryPath: fixture.repository, IssueNumber: 1, Stage: store.StageClaim, Status: status, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(100, 0).UTC()}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	identityFn, lookPath, imagesFn, installedFn := upgradeIdentityForCLI, upgradeLookPathForCLI, upgradeImagesForCLI, upgradeInstalledIdentityForCLI
	t.Cleanup(func() {
		upgradeIdentityForCLI, upgradeLookPathForCLI, upgradeImagesForCLI, upgradeInstalledIdentityForCLI = identityFn, lookPath, imagesFn, installedFn
	})
	upgradeInstalledIdentityForCLI = func(context.Context, string) (version.Identity, error) {
		return version.Identity{SchemaVersion: 1, Version: "v1.3.0", Build: "release", Revision: "abc123"}, nil
	}
	upgradeIdentityForCLI = func() version.Identity {
		return version.Identity{SchemaVersion: 1, Version: "v1.4.0", Build: "release", Revision: "abc123", WorkerBaseImage: "ghcr.io/stevie1704/sw-factory-base@sha256:" + sixtyFour("1")}
	}
	upgradeLookPathForCLI = func(string) (string, error) { return "/opt/factory-v1.3.0/bin/factory", nil }
	upgradeImagesForCLI = func() upgradeImages { return images }
	return fixture
}

// sixtyFour repeats one hexadecimal digit to a digest length.
func sixtyFour(digit string) string {
	return string(bytes.Repeat([]byte(digit), 64))
}

// upgradeDocument is the part of the upgrade plan document the tests read.
type upgradeDocument struct {
	SchemaVersion int      `json:"schema_version"`
	Command       string   `json:"command"`
	Result        string   `json:"result"`
	StoreBackups  []string `json:"store_backups"`
	// StoreIncompleteBackups is read by the incomplete-backup test.
	StoreIncompleteBackups []string `json:"store_incomplete_backups"`
	Components             []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"components"`
	Blockers []struct {
		Code string `json:"code"`
	} `json:"blockers"`
	Operations []struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Command string `json:"command"`
	} `json:"operations"`
	RecoveryLimits []string `json:"recovery_limits"`
}

// runUpgradeCommand runs one upgrade command and decodes its document.
func runUpgradeCommand(t *testing.T, document any, args ...string) int {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	code := Run(context.Background(), append([]string{"upgrade"}, args...), &output, &errorsOutput)
	if document != nil {
		if err := json.Unmarshal(output.Bytes(), document); err != nil {
			t.Fatalf("decode %q (stderr %q): %v", output.String(), errorsOutput.String(), err)
		}
	}
	return code
}

// snapshotFiles returns the content digest of every file below directory.
func snapshotFiles(t *testing.T, directory string) map[string][32]byte {
	t.Helper()
	files := map[string][32]byte{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		files[path] = sha256.Sum256(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestUpgradePlanReportsAnIncompleteBackupApart verifies that a backup
// file without a matching provenance file is never listed as a backup, but is
// reported as incomplete.
func TestUpgradePlanReportsAnIncompleteBackupApart(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.3.0"})
	partial := fixture.store + ".bak-20260101T000000.000000000Z"
	if err := os.WriteFile(partial, []byte("partial database"), 0o600); err != nil {
		t.Fatal(err)
	}

	var document upgradeDocument
	code := runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--json")

	if code != 0 || len(document.StoreBackups) != 0 || len(document.StoreIncompleteBackups) != 1 || document.StoreIncompleteBackups[0] != partial {
		t.Fatalf("plan = exit %d, backups %v, incomplete %v; want only %s as incomplete", code, document.StoreBackups, document.StoreIncompleteBackups, partial)
	}
}

// TestUpgradePlanOfAnIdleInstallationChangesNothing verifies the read-only
// contract: a reviewable, ready plan, and no change to the store directory,
// the host configuration, or the repository.
func TestUpgradePlanOfAnIdleInstallationChangesNothing(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.3.0"})
	before := snapshotFiles(t, filepath.Dir(fixture.hostConfig))
	repositoryBefore := snapshotFiles(t, fixture.repository)

	var document upgradeDocument
	code := runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--json")

	if code != 0 || document.Command != "upgrade plan" || document.Result != "ready" || len(document.Blockers) != 0 {
		t.Fatalf("plan = exit %d, %+v; want a ready plan", code, document)
	}
	states := map[string]string{}
	for _, component := range document.Components {
		states[component.Name] = component.State
	}
	if states["store"] != "new" || states["worker"] != "old" || states["binary"] != "old" {
		t.Fatalf("components = %v, want a current store and an old worker and binary", states)
	}
	if len(document.Operations) != 10 || len(document.RecoveryLimits) == 0 || document.StoreBackups == nil {
		t.Fatalf("plan = %+v, want ten operations, recovery limits, and backups", document)
	}
	after := snapshotFiles(t, filepath.Dir(fixture.hostConfig))
	if len(after) != len(before) {
		t.Fatalf("files after the plan = %d, before = %d", len(after), len(before))
	}
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("the plan changed %s", path)
		}
	}
	for path, digest := range repositoryBefore {
		if snapshotFiles(t, fixture.repository)[path] != digest {
			t.Fatalf("the plan changed %s", path)
		}
	}
}

// TestUpgradePlanBlocksAnActiveRunAndAMissingBaseImage verifies the explicit
// blockers and the blocked exit status.
func TestUpgradePlanBlocksAnActiveRunAndAMissingBaseImage(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusActive, fakeUpgradeImages{release: "v1.3.0", missing: map[string]bool{"ghcr.io/stevie1704/sw-factory-base": true}})

	var document upgradeDocument
	code := runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--json")

	codes := map[string]bool{}
	for _, blocker := range document.Blockers {
		codes[blocker.Code] = true
	}
	if code != 1 || document.Result != "blocked" || !codes["run.active"] || !codes["worker.base_image_missing"] {
		t.Fatalf("plan = exit %d, %+v; want run.active and worker.base_image_missing", code, document)
	}
}

// TestUpgradePlanReusesTheOnboardingPlan verifies that the evidence of a
// named onboarding plan is evaluated against the target.
func TestUpgradePlanReusesTheOnboardingPlan(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	if code, _, stderr := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", fixture.hostConfig, "--resources", fixture.resources, "--json"); code != 0 {
		t.Fatalf("onboard plan exit = %d: %s", code, stderr)
	}

	var document upgradeDocument
	runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--onboarding-plan", fixture.plan, "--json")

	states := map[string]string{}
	for _, operation := range document.Operations {
		states[operation.ID] = operation.State
	}
	if states["prepare_resources"] != "done" || states["onboarding_plan"] != "done" || states["rebuild_worker"] != "done" {
		t.Fatalf("operations = %v, want reused prepare, plan, and worker evidence", states)
	}
}

// TestUpgradeMigrateStoreReportsTheBackupProvenance verifies that a
// migration names the pre-migration backup and its provenance file in the
// JSON document. This release accepts only its own schema, so a fake
// migration result stands in for the migration.
func TestUpgradeMigrateStoreReportsTheBackupProvenance(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	migrate := upgradeMigrateStoreForCLI
	t.Cleanup(func() { upgradeMigrateStoreForCLI = migrate })
	upgradeMigrateStoreForCLI = func(context.Context, config.RepositoryRegistration, factory.WorkerProbe) (factory.StoreMigrationResult, error) {
		return factory.StoreMigrationResult{Migration: store.Migration{FromVersion: 52, ToVersion: 53, Backup: "/data/factory.db.bak-1", Provenance: "/data/factory.db.bak-1.json"}}, nil
	}

	var document struct {
		Outcome   string `json:"outcome"`
		Migration struct {
			FromVersion int    `json:"from_version"`
			ToVersion   int    `json:"to_version"`
			Backup      string `json:"backup"`
			Provenance  string `json:"provenance"`
		} `json:"migration"`
	}
	code := runUpgradeCommand(t, &document, "migrate-store", "--config", fixture.hostConfig, "--json")

	if code != 0 || document.Outcome != "migrated" || document.Migration.FromVersion != 52 || document.Migration.ToVersion != 53 || document.Migration.Backup != "/data/factory.db.bak-1" || document.Migration.Provenance != "/data/factory.db.bak-1.json" {
		t.Fatalf("migrate-store = exit %d, %+v; want the migrated outcome with the backup and its provenance", code, document)
	}
}

// TestUpgradeMigrateStoreLeavesACurrentStoreUnchanged verifies the typed
// no-op result of the explicit entry point on a current store.
func TestUpgradeMigrateStoreLeavesACurrentStoreUnchanged(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})

	var document migrateStoreDocument
	code := runUpgradeCommand(t, &document, "migrate-store", "--config", fixture.hostConfig, "--json")

	if code != 0 || document.Outcome != "current" || document.Migration == nil || document.Migration.Backup != "" || document.Migration.ToVersion != store.CurrentSchemaVersion {
		t.Fatalf("migrate-store = exit %d, %+v; want the current outcome without a backup", code, document)
	}
}

// TestUpgradeMigrateStoreRefusesAnActiveRun verifies that the migration
// refuses active work and leaves the schema unchanged.
func TestUpgradeMigrateStoreRefusesAnActiveRun(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusActive, fakeUpgradeImages{release: "v1.3.0"})

	var document migrateStoreDocument
	code := runUpgradeCommand(t, &document, "migrate-store", "--config", fixture.hostConfig, "--json")

	if code != 1 || document.Outcome != "refused" || len(document.Refusals) == 0 || document.Refusals[0].Code != "run.active" {
		t.Fatalf("migrate-store = exit %d, %+v; want a run.active refusal", code, document)
	}
	inspection, err := store.InspectForUpgrade(context.Background(), fixture.store)
	if err != nil || inspection.SchemaVersion != store.CurrentSchemaVersion {
		t.Fatalf("schema after refusal = %d, %v", inspection.SchemaVersion, err)
	}
}

// TestUpgradeNeedsASubcommandAndJSON verifies the argument errors.
func TestUpgradeNeedsASubcommandAndJSON(t *testing.T) {
	for _, args := range [][]string{{}, {"apply"}, {"plan"}, {"migrate-store"}} {
		if code := runUpgradeCommand(t, nil, args...); code != 2 {
			t.Fatalf("upgrade %v exit = %d, want 2", args, code)
		}
	}
}

// TestUpgradePlanAdvancesAfterPrepare verifies that a directory that onboard
// prepare of the target filled proves prepare_resources before any
// onboarding plan exists.
func TestUpgradePlanAdvancesAfterPrepare(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.3.0"})

	var document upgradeDocument
	runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--resources", fixture.resources, "--json")

	states := map[string]string{}
	for _, operation := range document.Operations {
		states[operation.ID] = operation.State
	}
	if states["prepare_resources"] != "done" {
		t.Fatalf("operations = %v, want prepare_resources done", states)
	}
}

// TestUpgradePlanRefusesAnOnboardingPlanOfAnotherConfiguration verifies that
// a plan of the same checkout but another host configuration is a blocker.
func TestUpgradePlanRefusesAnOnboardingPlanOfAnotherConfiguration(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	other := filepath.Join(t.TempDir(), "other-config.yaml")
	if code, _, stderr := runPlanCommand("onboard", "plan", "--repository", fixture.repository, "--output", fixture.plan, "--config", other, "--resources", fixture.resources, "--json"); code != 0 {
		t.Fatalf("onboard plan exit = %d: %s", code, stderr)
	}

	var document upgradeDocument
	code := runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--onboarding-plan", fixture.plan, "--json")

	found := false
	for _, blocker := range document.Blockers {
		found = found || blocker.Code == "onboarding.plan_registration_mismatch"
	}
	if code != 1 || !found {
		t.Fatalf("plan = exit %d, %+v; want onboarding.plan_registration_mismatch", code, document.Blockers)
	}
}

// liveUpgradeWorkers is a Docker view in which every asked worker runs a
// live harness helper, or Docker fails with err.
type liveUpgradeWorkers struct{ err error }

// RunningWorkers reports every asked worker as running, or fails.
func (w liveUpgradeWorkers) RunningWorkers(_ context.Context, workerIDs []string) ([]string, error) {
	return workerIDs, w.err
}

// ObserveHeadless reports a running helper.
func (liveUpgradeWorkers) ObserveHeadless(context.Context, worker.HeadlessRequest) (worker.HeadlessStatus, error) {
	return worker.HeadlessStatusRunning, nil
}

// withEndedInvocation records a completed invocation of run-1, so the store
// knows one worker, and makes Docker report it through workers.
func withEndedInvocation(t *testing.T, storePath string, workers factory.WorkerProbe) {
	t.Helper()
	opened, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	invocation := store.Invocation{ID: "inv-1", RunID: "run-1", Harness: "codex", Role: "implementation", Stage: store.StageImplementation, Status: store.InvocationStatusCompleted}
	if err := opened.SaveInvocation(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	previous := upgradeWorkersForCLI
	t.Cleanup(func() { upgradeWorkersForCLI = previous })
	upgradeWorkersForCLI = func() factory.WorkerProbe { return workers }
}

// TestUpgradeRefusesALiveWorkerAndUnknownDocker verifies that a harness
// process in a worker of the store, with no active invocation, and an
// unreachable Docker daemon block the plan and refuse migrate-store.
func TestUpgradeRefusesALiveWorkerAndUnknownDocker(t *testing.T) {
	for code, workers := range map[string]factory.WorkerProbe{
		"worker.active":      liveUpgradeWorkers{},
		"docker.unreachable": liveUpgradeWorkers{err: errors.New("Cannot connect to the Docker daemon")},
	} {
		fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
		withEndedInvocation(t, fixture.store, workers)

		var plan upgradeDocument
		exit := runUpgradeCommand(t, &plan, "plan", "--config", fixture.hostConfig, "--json")
		blocked := false
		for _, blocker := range plan.Blockers {
			blocked = blocked || blocker.Code == code
		}
		if exit != 1 || !blocked {
			t.Fatalf("plan = exit %d, %+v; want a %s blocker", exit, plan, code)
		}
		var migration migrateStoreDocument
		exit = runUpgradeCommand(t, &migration, "migrate-store", "--config", fixture.hostConfig, "--json")
		if exit != 1 || migration.Outcome != "refused" || len(migration.Refusals) != 1 || migration.Refusals[0].Code != code {
			t.Fatalf("migrate-store = exit %d, %+v; want a %s refusal", exit, migration, code)
		}
	}
}
