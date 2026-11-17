package worker_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// harnessPackageStages maps each harness to the install stage that stages its
// native executable below /harness.
var harnessPackageStages = map[string]string{
	"claude":  "claude-package",
	"codex":   "codex-package",
	"copilot": "copilot-package",
}

// baseVariants maps each published base image target to the harnesses its
// label names and the only harness stages it may copy from.
var baseVariants = map[string][]string{
	"harness-codex":   {"codex"},
	"harness-claude":  {"claude"},
	"harness-copilot": {"copilot"},
	"harness-all":     {"claude", "codex", "copilot"},
}

// dockerStage is one FROM block of a Dockerfile.
type dockerStage struct {
	name, from, body string
}

// TestBaseDefinitionBuildsEveryHarnessVariant verifies the base definition
// has one target per published variant, that each variant labels exactly
// its harnesses, and that a plain build without --target produces the
// combined image the release names as worker_base_image.
func TestBaseDefinitionBuildsEveryHarnessVariant(t *testing.T) {
	stages := baseStages(t)
	if last := stages[len(stages)-1].name; last != "harness-all" {
		t.Fatalf("the default (last) target is %q, want harness-all", last)
	}
	for target, harnesses := range baseVariants {
		labels := regexp.MustCompile(`(?m)^LABEL io\.github\.stevie1704\.sw-factory\.harnesses="([^"]*)"$`).FindAllStringSubmatch(stageNamed(t, stages, target).body, -1)
		if len(labels) != 1 || labels[0][1] != strings.Join(harnesses, " ") {
			t.Errorf("%s harness labels = %v, want one naming %q", target, labels, strings.Join(harnesses, " "))
		}
	}
}

// TestBaseVariantsCarryNoNodeOrGoToolchain verifies every published variant
// derives from the minimal runtime stage, never from the Node or Go images
// that only the build stages use, and takes nothing from those stages except
// the compiled helpers and the staged native harness files.
func TestBaseVariantsCarryNoNodeOrGoToolchain(t *testing.T) {
	stages := baseStages(t)
	if runtime := stageNamed(t, stages, "runtime"); runtime.from != "debian:bookworm-slim" {
		t.Fatalf("runtime stage derives from %q, want the minimal debian:bookworm-slim without Node or Go", runtime.from)
	}
	allowedCopies := regexp.MustCompile(`^COPY --from=(factory-report-builder /factory-(report|worker-headless) |(claude|codex|copilot)-package /harness/ /$)`)
	for target := range baseVariants {
		for stage := stageNamed(t, stages, target); ; stage = stageNamed(t, stages, stage.from) {
			for _, line := range strings.Split(stage.body, "\n") {
				if strings.HasPrefix(line, "COPY --from=") && !allowedCopies.MatchString(line) {
					t.Errorf("%s (via %s) copies more than the helpers and staged harness files: %s", target, stage.name, line)
				}
			}
			if stage.name == "runtime" {
				break
			}
		}
	}
}

// TestSingleHarnessVariantsCopyOnlyTheirOwnHarness verifies a single-harness
// variant contains no package of another harness.
func TestSingleHarnessVariantsCopyOnlyTheirOwnHarness(t *testing.T) {
	stages := baseStages(t)
	for target, harnesses := range baseVariants {
		var copied []string
		for stage := stageNamed(t, stages, target); stage.name != "runtime"; stage = stageNamed(t, stages, stage.from) {
			for harness, packageStage := range harnessPackageStages {
				if strings.Contains(stage.body, "COPY --from="+packageStage+" ") {
					copied = append(copied, harness)
				}
			}
		}
		slices.Sort(copied)
		if !slices.Equal(copied, harnesses) {
			t.Errorf("%s copies the harnesses %v, want %v", target, copied, harnesses)
		}
	}
}

// baseStages parses worker/base.Dockerfile into its FROM blocks.
func baseStages(t *testing.T) []dockerStage {
	t.Helper()
	from := regexp.MustCompile(`^FROM (\S+) AS (\S+)$`)
	var stages []dockerStage
	for _, line := range strings.Split(readRepositoryFile(t, "worker/base.Dockerfile"), "\n") {
		if match := from.FindStringSubmatch(line); match != nil {
			stages = append(stages, dockerStage{name: match[2], from: match[1]})
			continue
		}
		if len(stages) > 0 {
			stages[len(stages)-1].body += line + "\n"
		}
	}
	if len(stages) == 0 {
		t.Fatal("worker/base.Dockerfile declares no named stage")
	}
	return stages
}

// stageNamed returns the stage with one name, failing the test without one.
func stageNamed(t *testing.T, stages []dockerStage, name string) dockerStage {
	t.Helper()
	for _, stage := range stages {
		if stage.name == name {
			return stage
		}
	}
	t.Fatalf("worker/base.Dockerfile has no stage %q", name)
	return dockerStage{}
}
