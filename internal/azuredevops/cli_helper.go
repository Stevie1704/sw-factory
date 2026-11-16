package azuredevops

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"context"
)

// cliHelper contains a private, temporary Azure CLI extension. Azure CLI
// discovers it through its extension.dev_sources setting for that invocation;
// installed extensions and CLI-managed authentication remain in place.
//
//go:embed cli_helper/azext_factory_devops/*.py cli_helper/azext_factory_devops/azext_metadata.json cli_helper/factory_devops.egg-info/PKG-INFO
var cliHelper embed.FS

// helperEnvironment materializes only embedded source in an owner-only
// directory. It changes no installed extension or credential configuration.
func helperEnvironment() ([]string, func(), error) {
	root, err := os.MkdirTemp("", "factory-azure-cli-*")
	if err != nil {
		return nil, nil, fmt.Errorf("prepare the Azure CLI helper: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	source, err := fs.Sub(cliHelper, "cli_helper")
	if err == nil {
		err = fs.WalkDir(source, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			target := filepath.Join(root, "factory-devops", filepath.FromSlash(path))
			if entry.IsDir() {
				return os.MkdirAll(target, 0o700)
			}
			content, readErr := fs.ReadFile(source, path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(target, content, 0o600)
		})
	}
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("prepare the Azure CLI helper: %w", err)
	}
	return []string{"AZURE_EXTENSION_DEV_SOURCES=" + root, "PYTHONDONTWRITEBYTECODE=1"}, cleanup, nil
}

// extensionPermission evaluates effective permissions using the official
// extension's SDK inside the CLI process. No subject or secret is supplied.
func (c *Client) extensionPermission(ctx context.Context, organization string, required permission, token string) (bool, error) {
	var response struct {
		Granted *bool `json:"granted"`
	}
	err := c.extension(ctx, organization, []string{"factory-devops", "permission", "--namespace-id", required.namespace, "--permissions", strconv.Itoa(required.bit), "--token", token}, &response)
	if err != nil {
		return false, err
	}
	if response.Granted == nil {
		return false, fmt.Errorf("Azure DevOps returned no effective permission result")
	}
	return *response.Granted, nil
}
