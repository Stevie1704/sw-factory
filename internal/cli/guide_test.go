package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/guide"
)

// TestRunVersionMarksTestBuildAsDevelopment verifies that a binary without
// release identity reports itself as a development build in text and JSON,
// and that both forms name the same build.
func TestRunVersionMarksTestBuildAsDevelopment(t *testing.T) {
	var text, textErrors bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version"}, &text, &textErrors); code != 0 {
		t.Fatalf("version exit code = %d, stderr = %q", code, textErrors.String())
	}
	if !strings.HasPrefix(text.String(), "factory development build (revision unknown)") {
		t.Fatalf("version output = %q, want a development build with an unknown revision", text.String())
	}

	var encoded, encodedErrors bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version", "--json"}, &encoded, &encodedErrors); code != 0 {
		t.Fatalf("version --json exit code = %d, stderr = %q", code, encodedErrors.String())
	}
	var identity map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &identity); err != nil {
		t.Fatalf("version --json output %q is not JSON: %v", encoded.String(), err)
	}
	want := map[string]any{
		"schema_version": float64(1),
		"version":        "development",
		"build":          "development",
		"revision":       "unknown",
		"modified":       false,
	}
	for field, value := range want {
		if identity[field] != value {
			t.Fatalf("version --json %s = %v, want %v (output %q)", field, identity[field], value, encoded.String())
		}
	}
}

// TestRunVersionRejectsPositionalArguments verifies the usage exit status.
func TestRunVersionRejectsPositionalArguments(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version", "extra"}, &output, &errorsOutput); code != 2 {
		t.Fatalf("version extra exit code = %d, want 2", code)
	}
}

// TestRunVersionAndGuideIgnoreConfigurationResolution verifies that guide and
// version run when no host configuration directory can be resolved.
func TestRunVersionAndGuideIgnoreConfigurationResolution(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("FACTORY_CONFIG", "")
	for _, args := range [][]string{{"version"}, {"guide"}} {
		var output, errorsOutput bytes.Buffer
		if code := cli.Run(context.Background(), args, &output, &errorsOutput); code != 0 {
			t.Fatalf("%v exit code = %d, stderr = %q", args, code, errorsOutput.String())
		}
	}
}

// TestRunGuidePrintsOverviewWithReleaseIdentity verifies the default guide
// names the installed build and indexes the topics.
func TestRunGuidePrintsOverviewWithReleaseIdentity(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"guide"}, &output, &errorsOutput); code != 0 {
		t.Fatalf("guide exit code = %d, stderr = %q", code, errorsOutput.String())
	}
	for _, want := range []string{"Installed release: factory development build (revision unknown)", "# Software Factory guide", "factory guide debug"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("guide output does not contain %q:\n%s", want, output.String())
		}
	}
}

// TestRunGuidePrintsOneTopic verifies a named topic is printed on its own.
func TestRunGuidePrintsOneTopic(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"guide", "debug"}, &output, &errorsOutput); code != 0 {
		t.Fatalf("guide debug exit code = %d, stderr = %q", code, errorsOutput.String())
	}
	if !strings.Contains(output.String(), "# Debugging") || strings.Contains(output.String(), "# Software Factory guide") {
		t.Fatalf("guide debug output is not the debug topic alone:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "Installed release: factory development build") {
		t.Fatalf("guide debug output does not name the installed release:\n%s", output.String())
	}
}

// TestRunGuideRejectsUnknownTopicWithDiscovery verifies the usage status and
// that the error names every available topic.
func TestRunGuideRejectsUnknownTopicWithDiscovery(t *testing.T) {
	var output, errorsOutput bytes.Buffer
	if code := cli.Run(context.Background(), []string{"guide", "nonexistent"}, &output, &errorsOutput); code != 2 {
		t.Fatalf("guide nonexistent exit code = %d, want 2", code)
	}
	for _, want := range []string{`unknown guide topic "nonexistent"`, "debug, reporting, setup", "factory guide"} {
		if !strings.Contains(errorsOutput.String(), want) {
			t.Fatalf("guide nonexistent stderr = %q, want %q", errorsOutput.String(), want)
		}
	}
}

// TestRunAdvertisesGuideInHelp verifies that missing commands, unknown
// commands, top-level help, and command flag help name the guide.
func TestRunAdvertisesGuideInHelp(t *testing.T) {
	for _, test := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"nonexistent"}, 2},
		{[]string{"--help"}, 0},
		{[]string{"doctor", "-h"}, 2},
	} {
		var output, errorsOutput bytes.Buffer
		code := cli.Run(context.Background(), test.args, &output, &errorsOutput)
		if code != test.code {
			t.Fatalf("%v exit code = %d, want %d", test.args, code, test.code)
		}
		if !strings.Contains(output.String()+errorsOutput.String(), "factory guide") {
			t.Fatalf("%v output does not name factory guide: stdout=%q stderr=%q", test.args, output.String(), errorsOutput.String())
		}
	}
}

// TestGuideNamesOnlyImplementedCommands verifies that every factory command
// the guide names exists in this release.
func TestGuideNamesOnlyImplementedCommands(t *testing.T) {
	command := regexp.MustCompile("(?m)(?:`|^)factory ([a-z][a-z-]*)")
	for _, name := range append([]string{guide.Overview}, guide.Topics()...) {
		body, err := guide.Topic(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range command.FindAllStringSubmatch(body, -1) {
			var output, errorsOutput bytes.Buffer
			cli.Run(context.Background(), []string{match[1], "-h"}, &output, &errorsOutput)
			if strings.Contains(errorsOutput.String(), "unknown command") {
				t.Errorf("topic %q names unimplemented command %q", name, match[1])
			}
		}
	}
}
