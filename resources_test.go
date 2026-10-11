package swfactory_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	swfactory "github.com/Stevie1704/sw-factory"
)

// TestExportResourcesWritesTheOnboardingResources proves that an export into a
// path with spaces carries the procedure, its reference documents, the
// curated worker skills, and executable scripts, byte for byte as checked in.
func TestExportResourcesWritesTheOnboardingResources(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "factory resources")

	if err := swfactory.ExportResources(destination); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"docs/repository-initialization.md",
		"docs/configuration.md",
		"docs/worker-runtime.md",
		"worker/SKILLS.md",
		"worker/Dockerfile",
		"worker/skills/implement/SKILL.md",
		"worker/skills/pr/SKILL.md",
		"scripts/smoke-skills.sh",
		"scripts/verify-headless-worker.sh",
	} {
		want, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("export lacks %s: %v", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("exported %s differs from the checked-in file", path)
		}
	}
	for _, script := range []string{"scripts/smoke-skills.sh", "scripts/verify-headless-worker.sh"} {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(script)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("exported %s is not executable: %v", script, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "factory.yaml")); !os.IsNotExist(err) {
		t.Errorf("export carries factory.yaml, which would point the smoke at Factory's own worker: %v", err)
	}
}

// TestExportResourcesRefusesToMixWithExistingFiles proves that an export never
// overwrites a file, so release resources cannot silently mix with another
// release's or a hand-edited copy.
func TestExportResourcesRefusesToMixWithExistingFiles(t *testing.T) {
	destination := t.TempDir()
	existing := filepath.Join(destination, "scripts", "smoke-skills.sh")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swfactory.ExportResources(destination); err == nil {
		t.Fatal("export over an existing file succeeded")
	}
	if got, _ := os.ReadFile(existing); string(got) != "#!/bin/sh\n" {
		t.Errorf("export changed the existing file to %q", got)
	}
}
