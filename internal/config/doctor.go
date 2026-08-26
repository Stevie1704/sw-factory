package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// DoctorState contains the validated configuration projections needed by
// other startup-check contributors. Invalid projections remain nil so the
// coordinator can report dependent failures without guessing values.
type DoctorState struct {
	// ConfigPath is the host configuration path being diagnosed.
	ConfigPath string
	// Host is the validated host configuration when available.
	Host HostConfig
	// Registration is the one registered repository when available.
	Registration *RepositoryRegistration
	// Repository is the checked-in repository policy when available.
	Repository *RepositoryConfig
	problem    string
	action     string
}

// StartupCheck loads and validates host-local and repository configuration
// once, returning the safe state used to compose the remaining checks.
func StartupCheck(path string) (DoctorState, doctor.Check) {
	state := inspectDoctorState(path)
	return state, func(context.Context) doctor.Result {
		if state.problem != "" {
			return doctor.Failure("configuration", state.problem, state.action)
		}
		return doctor.Success("configuration")
	}
}

// inspectDoctorState performs the configuration-owned read-only diagnosis.
func inspectDoctorState(path string) DoctorState {
	state := DoctorState{ConfigPath: path}
	if strings.TrimSpace(path) == "" {
		state.problem = "host-local configuration path is not configured"
		state.action = "set FACTORY_CONFIG or pass --config to a readable host configuration"
		return state
	}
	info, err := os.Lstat(path)
	if err != nil {
		state.problem = "host-local configuration cannot be read"
		state.action = "run factory init for the configured host configuration path"
		return state
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		state.problem = "host-local configuration is not a regular file"
		state.action = "replace the host configuration with a regular private file"
		return state
	}
	if info.Mode().Perm()&0o077 != 0 {
		state.problem = "host-local configuration is not private"
		state.action = "restrict the host configuration to owner read/write permissions"
		return state
	}
	host, err := LoadHost(path)
	if err != nil {
		state.problem = "host-local configuration is missing or invalid"
		state.action = "repair the host configuration schema and registration fields"
		return state
	}
	state.Host = host
	if len(host.Repositories) == 0 {
		state.problem = "no repository is registered"
		state.action = "run factory register for the repository this coordinator should operate"
		return state
	}
	registration := host.Repositories[0]
	state.Registration = &registration
	if !pathWithin(registration.Path, registration.RepositoryConfigPath) {
		state.problem = "repository configuration is outside the registered repository"
		state.action = "point repository_config_path at the checked-in factory.yaml inside the repository"
		return state
	}
	configInfo, err := os.Lstat(registration.RepositoryConfigPath)
	if err != nil || configInfo.Mode()&os.ModeSymlink != 0 || !configInfo.Mode().IsRegular() {
		state.problem = "checked-in repository configuration is not a regular file"
		state.action = "restore a regular factory.yaml inside the registered repository checkout"
		return state
	}
	repository, err := LoadRepository(registration.RepositoryConfigPath)
	if err != nil {
		state.problem = "checked-in repository configuration is missing or invalid"
		state.action = "repair the repository factory.yaml and its declared workflow policy"
		return state
	}
	state.Repository = &repository
	return state
}

// pathWithin reports whether target is equal to or below root after resolving
// existing symlinks. Registration validation already requires absolute paths;
// the repeated check protects diagnosis from a hand-edited host file.
func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(resolvePath(root), resolvePath(target))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// resolvePath resolves existing path components while preserving unresolved
// trailing components, so a not-yet-created target can still be checked.
func resolvePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	suffix := []string{}
	current := absolute
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
