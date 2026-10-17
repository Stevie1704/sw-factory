package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestHarnessExecutableProbeAcceptsOnlyThePinnedCopilotRelease runs the
// in-image probe against a fake copilot and verifies it accepts only the
// release the compatibility record proved.
func TestHarnessExecutableProbeAcceptsOnlyThePinnedCopilotRelease(t *testing.T) {
	for _, test := range []struct {
		name, version string
		usable        bool
	}{
		{name: "pinned release", version: CopilotVersionLine, usable: true},
		{name: "other release", version: "GitHub Copilot CLI 1.0.99.", usable: false},
		{name: "no version", version: "", usable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\nprintf '%s\\n' '" + test.version + "'\nprintf 'Run copilot update to check for updates.\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "copilot"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/bin/sh", "-c", harnessExecutableProbe("copilot"))
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			if err := command.Run(); (err == nil) != test.usable {
				t.Fatalf("probe with %q = %v, want usable %t", test.version, err, test.usable)
			}
		})
	}
}

// TestCopilotVersionLineNamesTheProvedRelease verifies the doctor pin is the
// release of the compatibility record.
func TestCopilotVersionLineNamesTheProvedRelease(t *testing.T) {
	var record struct {
		CLI struct {
			Version string `json:"version"`
		} `json:"cli"`
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "spikes", "copilot-cli-1.0.98", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if want := "GitHub Copilot CLI " + record.CLI.Version + "."; CopilotVersionLine != want {
		t.Fatalf("CopilotVersionLine = %q, want %q", CopilotVersionLine, want)
	}
}
