package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

func TestRunInitializesRegistersAndReportsStatus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	operationalPath := filepath.Join(root, "state", "factory.db")

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "created host configuration") {
		t.Fatalf("init output = %q", output.String())
	}

	output.Reset()
	if code := cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", repositoryPath,
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "alice",
		"--operational-data", operationalPath,
	}, &output, &output); code != 0 {
		t.Fatalf("register exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	if code := cli.Run(context.Background(), []string{"status", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("status exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "active run: none") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"unknown"}, &output, &output); code == 0 {
		t.Fatal("unknown command returned success")
	}
	if !strings.Contains(output.String(), "unknown command") {
		t.Fatalf("output = %q", output.String())
	}
}
