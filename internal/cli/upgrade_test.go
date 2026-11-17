package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
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

// upgradeFixture is a registered repository with a previous-schema store.
type upgradeFixture struct {
	planFixture
	store string
}

// newUpgradeFixture registers the onboarding fixture, gives it a store with
// one run of the given status at the previous schema, and makes the target
// a release binary that PATH does not name yet.
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
	database, err := sql.Open("sqlite", fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	identityFn, lookPath, imagesFn := upgradeIdentityForCLI, upgradeLookPathForCLI, upgradeImagesForCLI
	t.Cleanup(func() {
		upgradeIdentityForCLI, upgradeLookPathForCLI, upgradeImagesForCLI = identityFn, lookPath, imagesFn
	})
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
	Components    []struct {
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
	if states["store"] != "old" || states["worker"] != "old" || states["binary"] != "old" {
		t.Fatalf("components = %v, want old store, worker, and binary", states)
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

// TestUpgradeMigrateStoreIsTheExplicitEntryPoint verifies the migration and
// its provenance, and that a repeated call changes nothing.
func TestUpgradeMigrateStoreIsTheExplicitEntryPoint(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})

	var migrated migrateStoreDocument
	if code := runUpgradeCommand(t, &migrated, "migrate-store", "--config", fixture.hostConfig, "--json"); code != 0 {
		t.Fatalf("migrate-store exit = %d, %+v", code, migrated)
	}
	if migrated.Outcome != "migrated" || migrated.Migration == nil || migrated.Migration.Backup == "" || migrated.Migration.ToVersion != store.CurrentSchemaVersion {
		t.Fatalf("migrate-store = %+v, want a migration with a backup", migrated)
	}
	var again migrateStoreDocument
	if code := runUpgradeCommand(t, &again, "migrate-store", "--config", fixture.hostConfig, "--json"); code != 0 || again.Outcome != "current" {
		t.Fatalf("second migrate-store = exit %d, %+v; want current", code, again)
	}
	var plan upgradeDocument
	runUpgradeCommand(t, &plan, "plan", "--config", fixture.hostConfig, "--json")
	if len(plan.StoreBackups) != 1 || plan.StoreBackups[0] != migrated.Migration.Backup {
		t.Fatalf("plan backups = %v, want the migration backup", plan.StoreBackups)
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
	if err != nil || inspection.SchemaVersion != store.CurrentSchemaVersion-1 {
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
