package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// writeHostWithWorkerLimits writes one registered host configuration whose
// registration ends with the given worker_limits block.
func writeHostWithWorkerLimits(t *testing.T, limits string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `schema_version: 2
repositories:
  - path: /srv/repository
    github:
      owner: example
      repository: project
    authorized_users: [alice]
    polling:
      interval: 1m
      backoff: 5m
    operational_data_path: /srv/state/factory.db
    repository_config_path: /srv/repository/factory.yaml
` + limits
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadHostReadsWorkerLimits verifies the host configuration owns the
// worker limits and that omitted limits take the documented defaults.
func TestLoadHostReadsWorkerLimits(t *testing.T) {
	t.Parallel()

	host, err := config.LoadHost(writeHostWithWorkerLimits(t, `    worker_limits:
      memory: 4g
      cpus: 1.5
      pids: 2048
`))
	if err != nil {
		t.Fatalf("LoadHost() error = %v", err)
	}
	got := config.EffectiveWorkerLimits(host.Repositories[0].WorkerLimits)
	want := worker.ResourceLimits{Memory: "4g", CPUs: "1.5", PIDs: "2048", LogMaxSize: "10m", LogMaxFiles: "3"}
	if got != want {
		t.Fatalf("EffectiveWorkerLimits() = %#v, want %#v", got, want)
	}

	host, err = config.LoadHost(writeHostWithWorkerLimits(t, ""))
	if err != nil {
		t.Fatalf("LoadHost() without limits error = %v", err)
	}
	got = config.EffectiveWorkerLimits(host.Repositories[0].WorkerLimits)
	want = worker.ResourceLimits{Memory: "8g", CPUs: "4", PIDs: "4096", LogMaxSize: "10m", LogMaxFiles: "3"}
	if got != want {
		t.Fatalf("EffectiveWorkerLimits() without limits = %#v, want %#v", got, want)
	}
}

// TestLoadHostRejectsInvalidWorkerLimits verifies zero, negative, and
// unparseable limits fail with a field-level error.
func TestLoadHostRejectsInvalidWorkerLimits(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		limit string
		field string
	}{
		{limit: "memory: 0", field: "memory"},
		{limit: "memory: -1g", field: "memory"},
		{limit: "memory: plenty", field: "memory"},
		{limit: "cpus: 0", field: "cpus"},
		{limit: "cpus: -4", field: "cpus"},
		{limit: "cpus: all", field: "cpus"},
		{limit: "pids: 0", field: "pids"},
		{limit: "pids: -1", field: "pids"},
		{limit: "pids: many", field: "pids"},
		{limit: "log_max_size: 0", field: "log_max_size"},
		{limit: "log_max_size: big", field: "log_max_size"},
		{limit: "log_max_files: 0", field: "log_max_files"},
		{limit: "log_max_files: -3", field: "log_max_files"},
	} {
		_, err := config.LoadHost(writeHostWithWorkerLimits(t, "    worker_limits:\n      "+test.limit+"\n"))
		var validationErr *config.ValidationError
		if !errors.As(err, &validationErr) || validationErr.Field != "repositories[0].worker_limits."+test.field {
			t.Errorf("LoadHost(%q) error = %v, want a repositories[0].worker_limits.%s error", test.limit, err, test.field)
		}
	}
}

// TestLoadRepositoryRejectsWorkerLimits verifies a repository commit cannot
// raise its own worker limits.
func TestLoadRepositoryRejectsWorkerLimits(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), config.RepositoryConfigFileName)
	if err := os.WriteFile(path, []byte("schema_version: 1\nworker_limits:\n  memory: 64g\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadRepository(path)
	if err == nil || !strings.Contains(err.Error(), "worker_limits") {
		t.Fatalf("LoadRepository() error = %v, want worker_limits rejected", err)
	}
}
