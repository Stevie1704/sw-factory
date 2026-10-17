package worker_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBaseImagePinsTheProvedCopilotRelease verifies the base image and the
// worker build script default to the Copilot CLI release the compatibility
// record proved, and that the image installs it through the pinned npm
// package beside the other harnesses.
func TestBaseImagePinsTheProvedCopilotRelease(t *testing.T) {
	var record struct {
		CLI struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"cli"`
	}
	if err := json.Unmarshal([]byte(readRepositoryFile(t, "docs/spikes/copilot-cli-1.0.98/manifest.json")), &record); err != nil {
		t.Fatalf("compatibility manifest is not JSON: %v", err)
	}
	manifest := record.CLI
	if manifest.Package != "@github/copilot" || manifest.Version == "" {
		t.Fatalf("compatibility manifest names %q@%q, want the @github/copilot release", manifest.Package, manifest.Version)
	}
	base := readRepositoryFile(t, "worker/base.Dockerfile")
	if match := regexp.MustCompile(`(?m)^ARG COPILOT_VERSION=(\S+)$`).FindStringSubmatch(base); match == nil || match[1] != manifest.Version {
		t.Fatalf("base.Dockerfile COPILOT_VERSION default = %v, want the proved %s", match, manifest.Version)
	}
	if !strings.Contains(base, `"@github/copilot@${COPILOT_VERSION}"`) {
		t.Fatal("base.Dockerfile does not install the pinned @github/copilot package")
	}
	build := readRepositoryFile(t, "scripts/build-worker.sh")
	if !strings.Contains(build, "docs/spikes/copilot-cli-"+manifest.Version+"/manifest.json") ||
		!strings.Contains(build, `--build-arg "COPILOT_VERSION=$COPILOT_VERSION"`) {
		t.Fatalf("scripts/build-worker.sh does not pass the proved Copilot version %s from its manifest to the base build", manifest.Version)
	}
}

// TestWorkerBuildRefusesAnUnprovedCopilotRelease verifies a COPILOT_VERSION
// override that differs from the proved release stops the build before any
// Docker call, so the image check cannot compare the override with itself.
func TestWorkerBuildRefusesAnUnprovedCopilotRelease(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "docker-called")
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ntouch '"+record+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "scripts/build-worker.sh")
	command.Dir = root
	command.Env = []string{"PATH=/usr/bin:/bin", "DOCKER=" + docker, "COPILOT_VERSION=1.0.99"}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "COPILOT_VERSION=1.0.99 differs from the proved Copilot CLI") {
		t.Fatalf("build-worker.sh = %v, output %q; want a refusal of the unproved release", err, output)
	}
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatal("build-worker.sh called Docker before refusing the unproved release")
	}
}

// TestBaseImageInstallsTheCopilotLauncherAndManagedSettings verifies the base
// image ships the in-worker launcher the adapter calls and the image-owned
// settings it restores, and never ships the gh CLI, whose login would become
// a fallback Copilot credential.
func TestBaseImageInstallsTheCopilotLauncherAndManagedSettings(t *testing.T) {
	base := readRepositoryFile(t, "worker/base.Dockerfile")
	for _, want := range []string{
		"COPY worker/factory-copilot /usr/local/bin/factory-copilot",
		"COPY worker/copilot-settings.json /usr/local/share/factory-copilot/settings.json",
		"/home/factory/.copilot",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.Dockerfile is missing %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s+gh\s*\\?$`).MatchString(base) {
		t.Error("base.Dockerfile installs the gh CLI")
	}
}

// TestHostCopilotStateStaysOutOfWorkerBuilds verifies a host Copilot home in
// the checkout never enters the image build context or the sanitized gate
// checkout.
func TestHostCopilotStateStaysOutOfWorkerBuilds(t *testing.T) {
	ignore := readRepositoryFile(t, ".dockerignore")
	for _, want := range []string{"**/.copilot\n", "**/.copilot/**\n"} {
		if !strings.Contains(ignore, want) {
			t.Errorf(".dockerignore is missing %q", want)
		}
	}
	if !strings.Contains(readRepositoryFile(t, "scripts/build-worker.sh"), "--exclude='*/.copilot'") {
		t.Error("scripts/build-worker.sh copies a host .copilot directory into the gate checkout")
	}
}
