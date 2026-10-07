package worker

import (
	"context"
	"strconv"
	"strings"
)

// oomKillCountScript prints the worker cgroup's out-of-memory kill count from
// cgroup v2 memory.events or cgroup v1 memory.oom_control. It prints nothing
// when the kernel exposes no count. The value comes from the kernel, so no
// worker process can forge it.
const oomKillCountScript = `cat /sys/fs/cgroup/memory.events /sys/fs/cgroup/memory/memory.oom_control 2>/dev/null | awk '$1 == "oom_kill" { print $2; exit }'`

// OutOfMemoryError reports that the worker's memory limit killed a process
// while a command ran. It is an infrastructure failure and never a command
// result. It carries no command output, Docker name, or process ID.
type OutOfMemoryError struct{}

// Error names the cause and the corrective action.
func (e *OutOfMemoryError) Error() string {
	return "worker command was killed at the worker memory limit (out of memory); reduce the command's memory use or raise worker_limits.memory in the host configuration"
}

// oomKillCount reads the worker's out-of-memory kill count in its own Docker
// invocation, apart from any command output. It reports false when the count
// is unavailable, so a caller never infers a kill it cannot observe.
func (r *DockerRuntime) oomKillCount(ctx context.Context, workerID string) (int64, bool) {
	result, err := r.runDocker(ctx, []string{"exec", containerName(workerID), "/bin/sh", "-c", oomKillCountScript, "factory-oom-count"})
	if err != nil {
		return 0, false
	}
	count, err := strconv.ParseInt(strings.TrimSpace(result.Stdout), 10, 64)
	if err != nil {
		return 0, false
	}
	return count, true
}

// killedAtMemoryLimit reports whether the worker's out-of-memory kill count
// grew past a baseline read before the command started.
func (r *DockerRuntime) killedAtMemoryLimit(ctx context.Context, workerID string, before int64, known bool) bool {
	if !known {
		return false
	}
	after, ok := r.oomKillCount(ctx, workerID)
	return ok && after > before
}

// oomRecordPrelude selects the root-owned directory of out-of-memory
// baselines for this container boot. The worker user cannot create or change
// entries under /run, and the boot-scoped name keeps a baseline from an
// earlier container start from meeting a reset kill count.
const oomRecordPrelude = `root=/run/factory-oom
records="$root/$(cat /proc/sys/kernel/random/boot_id)-$(cut -d ' ' -f 22 /proc/1/stat)"
root_owned() { [ "$(stat -c %u "$1" 2>/dev/null)" = 0 ]; }
`

// recordOOMBaselineScript stores the current kill count for one detached
// invocation. It runs as root and refuses any path it does not own.
const recordOOMBaselineScript = oomRecordPrelude + `count=$(` + oomKillCountScript + `)
[ -n "$count" ] || exit 0
mkdir -p "$root" && root_owned "$root" || exit 1
mkdir -p "$records" && root_owned "$records" || exit 1
printf '%s\n' "$count" > "$records/.$1" && mv -f "$records/.$1" "$records/$1"
`

// checkOOMBaselineScript prints oom when the kill count grew past the
// invocation's root-owned baseline. It prints nothing when no trusted
// baseline exists.
const checkOOMBaselineScript = oomRecordPrelude + `file="$records/$1"
for path in "$root" "$records" "$file"; do root_owned "$path" || exit 0; done
baseline=$(cat "$file")
count=$(` + oomKillCountScript + `)
if [ -n "$count" ] && [ -n "$baseline" ] && [ "$count" -gt "$baseline" ]; then echo oom; fi
`

// recordOOMBaseline records the kill count before a detached process starts.
// It is best effort: without a baseline the process exit keeps its ordinary
// classification, and the launch is not refused.
func (r *DockerRuntime) recordOOMBaseline(ctx context.Context, workerID, invocationID string) {
	_, _ = r.runDocker(ctx, []string{"exec", "--user", "0:0", containerName(workerID), "/bin/sh", "-c", recordOOMBaselineScript, "factory-oom-baseline", invocationID})
}

// detachedProcessKilledAtMemoryLimit reports whether the kill count grew past
// the baseline recorded when the detached invocation started.
func (r *DockerRuntime) detachedProcessKilledAtMemoryLimit(ctx context.Context, workerID, invocationID string) bool {
	result, err := r.runDocker(ctx, []string{"exec", containerName(workerID), "/bin/sh", "-c", checkOOMBaselineScript, "factory-oom-check", invocationID})
	return err == nil && strings.TrimSpace(result.Stdout) == "oom"
}
