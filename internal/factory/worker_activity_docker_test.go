package factory_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// readOnlyDockerStub writes a Docker stub in which the container of
// containerName runs. Its exec runs the worker shell command on the host,
// with the helper state root mapped to stateRoot. A call of the worker
// helper is recorded in the returned log and fails, because the helper's
// inspect command can write the state.
func readOnlyDockerStub(t *testing.T, containerName, stateRoot string) (stub, helperLog string) {
	t.Helper()
	directory := t.TempDir()
	stub, helperLog = filepath.Join(directory, "docker"), filepath.Join(directory, "helper.log")
	script := `#!/bin/sh
case "$1" in
ps) echo ` + containerName + `; exit 0 ;;
container) printf 'true\timage\n'; exit 0 ;;
exec) ;;
*) exit 0 ;;
esac
for argument in "$@"; do
	case "$argument" in *factory-worker-headless*) echo helper >> '` + helperLog + `'; exit 1 ;; esac
done
while [ "$1" != /bin/sh ]; do
	shift
	[ $# -gt 0 ] || exit 3
done
exec /bin/sh -c "$3" "$4" '` + stateRoot + `'/"${5##*/}"
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub, helperLog
}

// deadPID returns the identifier of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	command := exec.Command("true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return command.Process.Pid
}

// TestWorkerActivityFindingsReadsHelperStateWithoutWriting verifies that
// the worker-activity check of upgrade plan and restore preview classifies
// the helper state through Docker without the helper's inspect command, and
// leaves the state files unchanged, also for a vanished process that the
// helper would record as lost.
func TestWorkerActivityFindingsReadsHelperStateWithoutWriting(t *testing.T) {
	process := store.WorkerProcess{SessionID: "s1", InvocationID: "sinv-1", Role: workflow.RoleIdeation, WorkerID: "session-s1-ideation"}
	digest := sha256.Sum256([]byte(process.WorkerID))
	containerName := "factory-worker-" + hex.EncodeToString(digest[:])[:24]
	live := strconv.Itoa(os.Getpid())
	dead := strconv.Itoa(deadPID(t))
	for _, test := range []struct {
		name, status, pid string
		active            bool
	}{
		{name: "running with a live process", status: "running", pid: live, active: true},
		{name: "running with a vanished process", status: "running", pid: dead},
		{name: "starting before the pid exists", status: "starting", active: true},
		{name: "cancelled with a live process", status: "cancelled", pid: live, active: true},
		{name: "cancelled with a vanished process", status: "cancelled", pid: dead},
		{name: "exited", status: "exited", pid: dead},
	} {
		stateRoot := t.TempDir()
		stateDir := filepath.Join(stateRoot, process.InvocationID)
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"status": test.status}
		if test.pid != "" {
			files["pid"] = test.pid
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(stateDir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stub, helperLog := readOnlyDockerStub(t, containerName, stateRoot)

		findings := factory.WorkerActivityFindings(context.Background(), []store.WorkerProcess{process}, &worker.DockerRuntime{DockerBinary: stub})

		if _, err := os.Stat(helperLog); err == nil {
			t.Fatalf("%s: the check called the worker helper", test.name)
		}
		if active := len(findings) == 1 && findings[0].Code == factory.FindingWorkerActive; active != test.active || !test.active && len(findings) != 0 {
			t.Fatalf("%s: findings = %+v, want active %t", test.name, findings, test.active)
		}
		entries, err := os.ReadDir(stateDir)
		if err != nil || len(entries) != len(files) {
			t.Fatalf("%s: state entries = %v (%v), want %d files", test.name, entries, err, len(files))
		}
		for name, content := range files {
			if data, err := os.ReadFile(filepath.Join(stateDir, name)); err != nil || string(data) != content {
				t.Fatalf("%s: state file %s = %q (%v), want %q", test.name, name, data, err, content)
			}
		}
	}
}
