package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// commandRecordRoot is the worker-local parent of the per-boot command records.
// It lives on the container filesystem, so the records and the processes they
// name share the lifetime of one worker container.
const commandRecordRoot = "/tmp/factory-commands"

// commandRefusedMarker prefixes the line the supervisor prints when an earlier
// cancelled command is still alive. The adapter matches it together with the
// private per-invocation identity, so command output cannot forge it.
const commandRefusedMarker = "factory-command-refused "

// commandOutOfMemoryMarker prefixes the line the supervisor prints when the
// worker's memory limit killed a process while a failed command ran. Like the
// refusal marker, it is matched together with the private invocation identity.
const commandOutOfMemoryMarker = "factory-command-oom "

// commandSupervisionPrelude is shared by the supervisor and the terminator. It
// selects a records directory that changes whenever the container or the
// Docker host restarts, so a process-group number recorded before a restart
// can never name an unrelated process after it. alive ignores zombies: a
// worker started without an init process does not reap orphans, and a zombie
// cannot modify the checkout.
const commandSupervisionPrelude = `records="$1/$(cat /proc/sys/kernel/random/boot_id)-$(cut -d ' ' -f 22 /proc/1/stat)"
id=$2
alive() { ps -A -o pgid= -o stat= | awk -v group="$1" '$1 == group && $2 !~ /^Z/ { found = 1 } END { exit !found }'; }
oom_kills() { cat /sys/fs/cgroup/memory.events /sys/fs/cgroup/memory/memory.oom_control 2>/dev/null | awk '$1 == "oom_kill" { print $2; exit }'; }
`

// superviseCommandScript runs one command as the leader of a new process group
// and records that group atomically before the command starts. It refuses to
// start while a command whose cancellation began is still alive. A cancellation
// that wins the race against the record makes the leader exit before the
// command runs. When a failed command ran while the worker cgroup's
// out-of-memory kill count grew, it prints the out-of-memory marker last.
const superviseCommandScript = commandSupervisionPrelude + `if ! mkdir -p "$records" 2>/dev/null || ! command -v setsid >/dev/null 2>&1; then
  echo "worker command supervision is unavailable" >&2
  exit 125
fi
for cancel in "$records"/*.cancel; do
  [ -e "$cancel" ] || continue
  group=$(cat "${cancel%.cancel}.pgid" 2>/dev/null) || continue
  if alive "$group"; then
    echo "` + commandRefusedMarker + `$id" >&2
    exit 125
  fi
done
oom_before=$(oom_kills)
setsid -w /bin/sh -c '
printf "%s\n" "$$" > "$1/.$2.pgid" && mv -f "$1/.$2.pgid" "$1/$2.pgid" || exit 125
[ -e "$1/$2.cancel" ] && exit 143
exec /bin/sh -c "$3"
' factory-command-leader "$records" "$id" "$3"
status=$?
group=$(cat "$records/$id.pgid" 2>/dev/null) && ! alive "$group" && rm -f "$records/$id.pgid"
oom_after=$(oom_kills)
if [ "$status" -ne 0 ] && [ -n "$oom_before" ] && [ "${oom_after:-0}" -gt "$oom_before" ]; then
  echo "` + commandOutOfMemoryMarker + `$id" >&2
fi
exit $status
`

// terminateCommandScript cancels one supervised command. It publishes the
// cancellation before it reads the record, sends SIGTERM to the process group,
// escalates to SIGKILL after the grace period, and exits 3 when the group
// still has live members. It keeps both records in that case, so the
// supervisor refuses later commands until the worker is stopped or recreated.
const terminateCommandScript = commandSupervisionPrelude + `grace=$3
settle() {
  tries=$(($2 * 10))
  while alive "$1"; do
    [ "$tries" -gt 0 ] || return 1
    tries=$((tries - 1))
    sleep 0.1
  done
}
mkdir -p "$records" && : > "$records/$id.cancel" || exit 1
group=$(cat "$records/$id.pgid" 2>/dev/null) || exit 0
case $group in ''|*[!0-9]*|0|1) exit 1 ;; esac
kill -s TERM -- "-$group" 2>/dev/null
if ! settle "$group" "$grace"; then
  kill -s KILL -- "-$group" 2>/dev/null
  settle "$group" "$grace" || exit ` + terminateSurvivedExitCode + `
fi
rm -f "$records/$id.pgid" "$records/$id.cancel"
`

// terminateSurvivedExitCode is the terminator's exit code for a process group
// that survived SIGKILL. It is text because the terminator script embeds it.
const terminateSurvivedExitCode = "3"

// commandTerminationSlack bounds the Docker round trips around the two grace
// periods, so a lost cancellation response cannot block forever.
const commandTerminationSlack = 10 * time.Second

// commandTerminationGrace is the time allowed for a cancelled process group to
// handle SIGTERM, and again for it to disappear after SIGKILL. Real-Docker
// tests shorten it.
var commandTerminationGrace = 5 * time.Second

// CommandTerminationError reports that a cancelled worker command could not be
// proven stopped, or that the worker refused new work because an earlier
// cancelled command is still alive. It is an infrastructure discrepancy and
// never a command result: the worker must be stopped or recreated before it
// runs more work. It carries no command output, Docker name, or process ID.
type CommandTerminationError struct {
	// Reason is a bounded, content-free diagnosis in the seam's vocabulary.
	Reason string
}

// Error names the discrepancy and the recovery action.
func (e *CommandTerminationError) Error() string {
	return "worker command termination is unconfirmed: " + e.Reason + "; stop or recreate the worker before continuing"
}

// supervisedCommandArgs returns the in-worker argv that runs command under the
// supervisor with one private invocation identity.
func supervisedCommandArgs(commandID, command string) []string {
	return []string{"/bin/sh", "-c", superviseCommandScript, "factory-command", commandRecordRoot, commandID, command}
}

// newCommandID returns a private random identity for one supervised command.
func newCommandID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create worker command identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// terminateCommand stops one cancelled command inside the worker and returns
// nil only when no live member of its process group remains. The caller's
// cancellation does not apply here: termination must outlive the deadline that
// triggered it, but it stays bounded so a lost response cannot block forever.
func (r *DockerRuntime) terminateCommand(ctx context.Context, workerID, commandID string) error {
	timeout := 2*commandTerminationGrace + commandTerminationSlack
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	grace := strconv.Itoa(int(commandTerminationGrace.Seconds()))
	_, err := r.runDocker(ctx, []string{"exec", containerName(workerID), "/bin/sh", "-c", terminateCommandScript, "factory-command-terminate", commandRecordRoot, commandID, grace})
	if err == nil {
		return nil
	}
	if r.workerGone(ctx, workerID) {
		return nil
	}
	var commandErr *dockerCommandError
	if errors.As(err, &commandErr) && strconv.Itoa(commandErr.ExitCode) == terminateSurvivedExitCode && !isDockerRuntimeFailure(commandErr) {
		return &CommandTerminationError{Reason: "the command process group survived forced termination"}
	}
	return &CommandTerminationError{Reason: "the worker did not confirm the cancellation"}
}

// workerGone reports whether the worker is missing or stopped, which ends every
// process it ran. An inspection failure is not proof and reports false. It gets
// its own bound, because a lost terminator response may have used up ctx.
func (r *DockerRuntime) workerGone(ctx context.Context, workerID string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTerminationSlack)
	defer cancel()
	inspection, err := r.inspectContainer(ctx, containerName(workerID))
	if err != nil {
		return isContainerNotFound(err)
	}
	return !inspection.Running
}

// commandRefused reports whether the supervisor refused to start the command
// with commandID because an earlier cancelled command is still alive.
func commandRefused(commandErr *dockerCommandError, commandID string) bool {
	return commandErr.ExitCode == 125 && strings.Contains(commandErr.Stderr, commandRefusedMarker+commandID)
}

// commandOutOfMemory reports whether the supervisor saw the worker's memory
// limit kill a process while the failed command ran.
func commandOutOfMemory(commandErr *dockerCommandError, commandID string) bool {
	return strings.Contains(commandErr.Stderr, commandOutOfMemoryMarker+commandID)
}

// OutOfMemoryError reports that the worker's memory limit killed a process
// while a command ran. It is an infrastructure failure and never a command
// result. It carries no command output, Docker name, or process ID.
type OutOfMemoryError struct{}

// Error names the cause and the corrective action.
func (e *OutOfMemoryError) Error() string {
	return "worker command was killed at the worker memory limit (out of memory); reduce the command's memory use or raise worker_limits.memory in the host configuration"
}
