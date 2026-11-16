package azuredevops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIHelperSDKContract runs the Python tests without an Azure CLI
// installation, authentication, or a live service. They assert the narrow
// output and current-user effective-permission SDK arguments.
func TestCLIHelperSDKContract(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required to execute the embedded CLI helper tests")
	}
	command := exec.CommandContext(t.Context(), python, "-B", "-m", "unittest", "discover", "-s", "cli_helper_tests")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("CLI helper SDK tests: %v\n%s", err, output)
	}
}

// TestCLIHelperSourcesArePrivateAndRemoved checks the embedded extension
// layout that Azure CLI discovers, its permissions, and its cleanup.
func TestCLIHelperSourcesArePrivateAndRemoved(t *testing.T) {
	environment, cleanup, err := helperEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	root := strings.TrimPrefix(environment[0], "AZURE_EXTENSION_DEV_SOURCES=")
	path := filepath.Join(root, "factory-devops", "azext_factory_devops", "custom.py")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("helper source = %v, %v; want private source", info, err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("helper root = %v, %v; want private directory", info, err)
	}
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("helper directory remains after cleanup: %v", err)
	}
}
