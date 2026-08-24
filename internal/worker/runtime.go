// Package worker contains the portable worker-execution seam and its Docker
// adapter. Workflow code addresses workers by run identity only; Docker
// names, paths, and process details remain private to this package.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	// WorktreePath is the stable in-worker path for the run's writable checkout.
	WorktreePath = "/work"
	// GitMetadataPath is the stable in-worker path for read-only Git metadata.
	GitMetadataPath = "/git"
	// CachePath is the stable in-worker parent path for repository caches.
	CachePath = "/cache"
	// InvocationPath is the stable read-only path for a frozen invocation packet.
	InvocationPath = "/invocation"
	// ResultPath is the stable writable path for structured invocation reports.
	ResultPath = "/results"
	// CredentialPath is the stable worker path for the factory-managed Codex
	// credential volume. It is separate from the role session home.
	CredentialPath = "/run/factory-auth"
	// WorkerUser is the fixed non-root identity used for worker processes.
	WorkerUser = "10001:10001"
	// workerPrimaryGroupID is already present in WorkerUser and does not need
	// to be repeated as a supplemental Docker group.
	workerPrimaryGroupID = 10001
	// workerImageDigestPrefix is the only image digest algorithm accepted by
	// version one.
	workerImageDigestPrefix = "sha256:"
)

// EnvironmentPolicy controls which explicit environment a worker command may
// receive. The repository config selects clean or role for setup and gates;
// role commands additionally identify the workflow role that owns invocation.
type EnvironmentPolicy string

const (
	// EnvironmentPolicyClean removes ambient command environment and retains
	// only the worker baseline plus explicitly supplied values.
	EnvironmentPolicyClean EnvironmentPolicy = "clean"
	// EnvironmentPolicyRole identifies an explicit role environment while still
	// excluding ambient host variables and credentials.
	EnvironmentPolicyRole EnvironmentPolicy = "role"
)

// CacheMount describes one explicitly declared repository cache.
type CacheMount struct {
	// Name is the repository configuration name and becomes part of the stable
	// in-worker cache path.
	Name string
	// HostPath is the host directory mounted into the worker.
	HostPath string
	// ReadOnly controls whether the worker may update the cache.
	ReadOnly bool
}

// StartRequest contains the host-side inputs needed to create one worker.
// ImageDigest is mandatory so the worker cannot start from a mutable tag.
type StartRequest struct {
	// RunID identifies the workflow run and is the only worker identity exposed
	// to callers.
	RunID string
	// WorktreePath is the host path mounted read-write at /work.
	WorktreePath string
	// GitMetadataPath is the host Git metadata path mounted read-only at /git.
	GitMetadataPath string
	// Image is the repository worker image name without its digest.
	Image string
	// ImageDigest is the frozen sha256 image digest for the run.
	ImageDigest string
	// Caches are the only additional host paths made visible to the worker.
	Caches []CacheMount
	// InvocationPath is an optional host directory mounted read-only at
	// InvocationPath for one frozen invocation packet.
	InvocationPath string
	// ResultPath is an optional host directory mounted read-write at ResultPath
	// for one invocation's structured report.
	ResultPath string
	// CredentialStoreID identifies the factory-managed credential store. The
	// adapter derives a private persistent volume name from this value and
	// never exposes the value as a Docker identifier.
	CredentialStoreID string
	// Role selects the isolated role session volume used by the adapter.
	Role string
}

// ResumeRequest contains the immutable image identity needed to resume an
// existing worker after a coordinator or container interruption.
type ResumeRequest struct {
	// RunID selects the worker to resume without exposing its Docker name.
	RunID string
	// Image is the image name recorded for the run.
	Image string
	// ImageDigest is the digest that must match the existing worker.
	ImageDigest string
}

// CommandRequest describes one deterministic command executed inside a worker.
type CommandRequest struct {
	// RunID selects the worker that receives the command.
	RunID string
	// Command is interpreted by the worker's POSIX shell at the fixed /work path.
	Command string
	// EnvironmentPolicy selects clean or role execution.
	EnvironmentPolicy EnvironmentPolicy
	// Role names the coordinator-defined workflow role for the command.
	Role string
	// Environment contains explicit non-secret variables for this invocation.
	Environment map[string]string
}

// CommandResult reports a command's deterministic process result. A non-zero
// ExitCode is a command failure, not a worker-runtime failure.
type CommandResult struct {
	// ExitCode is the command's process exit code.
	ExitCode int
	// Stdout is the command's captured standard output.
	Stdout string
	// Stderr is the command's captured standard error.
	Stderr string
}

// InteractiveRequest describes one visible harness command. The adapter
// translates it into a host helper command without exposing a Docker name.
type InteractiveRequest struct {
	// RunID selects the worker receiving the interactive command.
	RunID string
	// Command is the harness executable and its arguments inside the worker.
	Command []string
	// EnvironmentPolicy selects clean or role execution.
	EnvironmentPolicy EnvironmentPolicy
	// Role names the coordinator-defined workflow role.
	Role string
	// Environment contains explicit non-secret invocation values.
	Environment map[string]string
}

// InteractiveCommand is a terminal-launch command whose executable runs on the
// host and attaches to the worker without revealing its runtime identifier.
type InteractiveCommand struct {
	// Executable is the host attach helper.
	Executable string
	// Args are helper arguments; the worker adapter owns runtime translation.
	Args []string
}

// CredentialSeedRequest selects one host-side Codex auth file to stream into a
// worker-owned role home. The host directory is never mounted.
type CredentialSeedRequest struct {
	// RunID selects the worker receiving the credential copy.
	RunID string
	// AuthPath is the explicit host path to one Codex auth.json file.
	AuthPath string
}

// NativeSessionRequest selects a harness-native session identifier lookup.
type NativeSessionRequest struct {
	// RunID selects the worker whose role home is inspected.
	RunID string
	// Harness identifies the session format being inspected.
	Harness string
}

// InteractiveRuntime is the optional extension implemented by runtimes that
// can provide a visible terminal attach command.
type InteractiveRuntime interface {
	WorkerRuntime
	// InteractiveCommand returns a command for a terminal adapter to launch.
	InteractiveCommand(context.Context, InteractiveRequest) (InteractiveCommand, error)
}

// CredentialSeeder is the optional extension for narrowly scoped auth seeding.
type CredentialSeeder interface {
	// SeedCodexCredentials streams one explicit Codex auth file into the worker.
	SeedCodexCredentials(context.Context, CredentialSeedRequest) error
}

// NativeSessionProvider is the optional extension for non-screen session
// identifier discovery.
type NativeSessionProvider interface {
	// NativeSessionID returns the newest known native session identifier.
	NativeSessionID(context.Context, NativeSessionRequest) (string, error)
}

// NativeSessionSnapshotProvider is the stronger session-discovery seam used
// for fresh launches. It lets a harness distinguish a session created by the
// current launch from an older persisted session.
type NativeSessionSnapshotProvider interface {
	// NativeSessionIDs returns all currently persisted native session
	// identifiers in stable adapter order.
	NativeSessionIDs(context.Context, NativeSessionRequest) ([]string, error)
}

// Inspection reports worker lifecycle state without exposing a runtime ID.
type Inspection struct {
	// Exists reports whether the run's worker container exists.
	Exists bool
	// Running reports whether the existing worker is currently running.
	Running bool
	// Image is the exact image reference recorded by the runtime, when present.
	Image string
	// Mounts are the container's configured bind mounts.
	Mounts []ContainerMount
	// mountIdentities maps stable in-worker destinations to adapter-private
	// mount identities used to detect stale bind or volume configuration.
	mountIdentities map[string]string
	// mountDestinations is the compatibility fallback for runtimes that expose
	// only destination names rather than Docker's structured inspect payload.
	mountDestinations map[string]struct{}
}

// ContainerMount describes one configured container bind mount.
type ContainerMount struct {
	// Source is the host path.
	Source string
	// Destination is the in-container path.
	Destination string
	// ReadOnly reports whether the mount is read-only.
	ReadOnly bool
}

// WorkerRuntime is the secondary product seam for isolated run execution.
// Implementations own container paths, runtime identifiers, role homes,
// invocation packets, result files, and process tracking.
type WorkerRuntime interface {
	// Start creates or reuses a worker from the frozen image digest.
	Start(context.Context, StartRequest) error
	// Resume starts an existing stopped worker without changing its image.
	Resume(context.Context, ResumeRequest) error
	// RunCommand executes one command inside the selected worker.
	RunCommand(context.Context, CommandRequest) (CommandResult, error)
	// Stop stops a worker while retaining it for a later resume.
	Stop(context.Context, string) error
	// Inspect reports whether the selected worker exists and is running.
	Inspect(context.Context, string) (Inspection, error)
}

// DockerRuntime implements WorkerRuntime with the host Docker executable.
// Docker container names are derived privately from RunID and never appear in
// the WorkerRuntime interface or its results.
type DockerRuntime struct {
	// DockerBinary overrides the executable name for controlled contract tests.
	// An empty value uses docker from PATH.
	DockerBinary string
	// AttachBinary is the host helper used to attach a cmux surface. An empty
	// value uses factory-worker-attach from PATH.
	AttachBinary string
}

// NewDockerRuntime creates a Docker-backed WorkerRuntime adapter.
func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{}
}

// Start creates a hardened worker from the exact image@digest reference. A
// running matching worker is reused, while a stopped matching worker is
// started in place when its mount contract matches the request.
func (r *DockerRuntime) Start(ctx context.Context, request StartRequest) error {
	if err := validateStartRequest(request); err != nil {
		return err
	}
	name := containerName(request.RunID)
	inspection, err := r.inspectContainer(ctx, name)
	if err == nil {
		if inspection.Image != imageReference(request.Image, request.ImageDigest) {
			return fmt.Errorf("worker %q uses image %q, want frozen image %q", request.RunID, inspection.Image, imageReference(request.Image, request.ImageDigest))
		}
		if !workerMountsPresent(request, inspection) {
			if err := r.recreateWorker(ctx, name, inspection.Running); err != nil {
				return fmt.Errorf("recreate worker %q for required mounts: %w", request.RunID, err)
			}
		} else if inspection.Running {
			return nil
		} else {
			if len(inspection.Mounts) > 0 {
				if err := validateMountContract(inspection.Mounts, request); err != nil {
					return fmt.Errorf("worker %q mount contract mismatch: %w", request.RunID, err)
				}
			}
			if _, err := prepareWorkerMounts(request); err != nil {
				return fmt.Errorf("prepare worker mounts: %w", err)
			}
			return r.startContainer(ctx, name)
		}
	} else if !isContainerNotFound(err) {
		return fmt.Errorf("inspect worker %q before start: %w", request.RunID, err)
	}
	groupIDs, err := prepareWorkerMounts(request)
	if err != nil {
		return fmt.Errorf("prepare worker mounts: %w", err)
	}

	args := []string{
		"run",
		"-d",
		"--name", name,
		"--user", WorkerUser,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--network", "bridge",
		"--workdir", WorktreePath,
		"--mount", bindMount(request.WorktreePath, WorktreePath, false),
		"--mount", bindMount(request.GitMetadataPath, GitMetadataPath, true),
	}
	if request.InvocationPath != "" {
		args = append(args, "--mount", bindMount(request.InvocationPath, InvocationPath, true))
	}
	if request.ResultPath != "" {
		args = append(args, "--mount", bindMount(request.ResultPath, ResultPath, false))
	}
	if request.CredentialStoreID != "" {
		args = append(args, "--mount", "type=volume,src="+credentialVolumeName(request.RunID, request.CredentialStoreID)+",dst="+CredentialPath)
	}
	role := request.Role
	if strings.TrimSpace(role) == "" {
		role = "implementation"
	}
	args = append(args, "--mount", "type=volume,src="+roleVolumeName(request.RunID, role)+",dst=/home/factory")
	for _, groupID := range groupIDs {
		args = append(args, "--group-add", strconv.Itoa(groupID))
	}
	for _, cache := range request.Caches {
		cachePath := filepath.Join(CachePath, cache.Name)
		args = append(args, "--mount", bindMount(cache.HostPath, cachePath, cache.ReadOnly))
	}
	for _, value := range baselineEnvironment(request.RunID) {
		args = append(args, "--env", value)
	}
	args = append(args, imageReference(request.Image, request.ImageDigest), "sleep", "infinity")
	if _, err := r.runDocker(ctx, args); err != nil {
		return fmt.Errorf("start worker %q: %w", request.RunID, err)
	}
	return nil
}

// Resume starts a stopped worker after verifying that its image remains the
// frozen image recorded for the run.
func (r *DockerRuntime) Resume(ctx context.Context, request ResumeRequest) error {
	if err := validateResumeRequest(request); err != nil {
		return err
	}
	name := containerName(request.RunID)
	inspection, err := r.inspectContainer(ctx, name)
	if err != nil {
		if isContainerNotFound(err) {
			return fmt.Errorf("worker %q does not exist and cannot be resumed", request.RunID)
		}
		return fmt.Errorf("inspect worker %q before resume: %w", request.RunID, err)
	}
	wantedImage := imageReference(request.Image, request.ImageDigest)
	if inspection.Image != wantedImage {
		return fmt.Errorf("worker %q uses image %q, want frozen image %q", request.RunID, inspection.Image, wantedImage)
	}
	if inspection.Running {
		return nil
	}
	return r.startContainer(ctx, name)
}

// RunCommand executes a shell command with an explicitly constructed worker
// environment. Host environment variables and credential-bearing names never
// enter the command environment.
func (r *DockerRuntime) RunCommand(ctx context.Context, request CommandRequest) (CommandResult, error) {
	if err := validateCommandRequest(request); err != nil {
		return CommandResult{}, err
	}
	inspection, err := r.inspectContainer(ctx, containerName(request.RunID))
	if err != nil {
		return CommandResult{}, fmt.Errorf("inspect worker %q before command: %w", request.RunID, err)
	}
	if !inspection.Running {
		return CommandResult{}, fmt.Errorf("worker %q is not running", request.RunID)
	}

	environment := commandEnvironment(request)
	args := []string{"exec", "--workdir", WorktreePath}
	for _, value := range environment {
		args = append(args, "--env", value)
	}
	args = append(args, containerName(request.RunID), "/usr/bin/env", "-i")
	args = append(args, environment...)
	args = append(args, "/bin/sh", "-c", request.Command)
	result, err := r.runDocker(ctx, args)
	if err == nil {
		return CommandResult{Stdout: result.Stdout, Stderr: result.Stderr}, nil
	}
	if ctx.Err() != nil {
		return CommandResult{}, fmt.Errorf("run command in worker %q: %w", request.RunID, ctx.Err())
	}
	var commandErr *dockerCommandError
	if errors.As(err, &commandErr) && commandErr.ProcessStarted && commandErr.ExitCode >= 0 && commandErr.ExitCode != 125 && !isDockerRuntimeFailure(commandErr) {
		return CommandResult{ExitCode: commandErr.ExitCode, Stdout: commandErr.Stdout, Stderr: commandErr.Stderr}, nil
	}
	return CommandResult{}, fmt.Errorf("run command in worker %q: %w", request.RunID, err)
}

// InteractiveCommand returns a host helper invocation for a visible terminal
// surface. Only the run/role identity and explicit non-secret protocol values
// cross the seam; the private Docker name is derived later inside the worker
// adapter/helper.
func (r *DockerRuntime) InteractiveCommand(_ context.Context, request InteractiveRequest) (InteractiveCommand, error) {
	if err := validateInteractiveRequest(request); err != nil {
		return InteractiveCommand{}, err
	}
	binary := r.AttachBinary
	if strings.TrimSpace(binary) == "" {
		binary = "factory-worker-attach"
	}
	args := []string{"--run-id", request.RunID, "--role", request.Role}
	entries := explicitEnvironment(request.Environment)
	for _, entry := range entries {
		args = append(args, "--env", entry)
	}
	args = append(args, "--")
	args = append(args, request.Command...)
	return InteractiveCommand{Executable: binary, Args: args}, nil
}

// SeedCodexCredentials streams one explicit auth.json file into the separate
// factory-managed credential volume and links only that file into Codex's role
// home. It never mounts the host file or returns credential contents.
func (r *DockerRuntime) SeedCodexCredentials(ctx context.Context, request CredentialSeedRequest) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if !filepath.IsAbs(request.AuthPath) || strings.ContainsAny(request.AuthPath, "\x00\r\n") {
		return errors.New("codex auth path must be an absolute safe path")
	}
	info, err := os.Lstat(request.AuthPath)
	if err != nil {
		return fmt.Errorf("inspect codex auth file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("codex auth path must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("codex auth path must be a regular file")
	}
	data, err := os.ReadFile(request.AuthPath)
	if err != nil {
		return fmt.Errorf("read codex auth file: %w", err)
	}
	if len(data) == 0 {
		return errors.New("codex auth file is empty")
	}
	// The worker drops every capability, so uid 0 holds neither CAP_CHOWN nor
	// CAP_DAC_OVERRIDE and is less able to write the factory-owned role home
	// than the worker user itself. Each step therefore runs as the owner of the
	// directory it writes, and no step changes ownership.
	name := containerName(request.RunID)
	if _, err := r.runDockerWithInput(ctx, []string{
		"exec", "-i", "--user", "0:0", "--workdir", WorktreePath,
		name, "/bin/sh", "-c",
		"umask 077; rm -f \"" + CredentialPath + "/auth.json\"; cat > \"" + CredentialPath + "/auth.json\"; chmod 0444 \"" + CredentialPath + "/auth.json\"",
	}, data); err != nil {
		return fmt.Errorf("seed Codex credentials: %w", dockerStderrDetail(err))
	}
	if _, err := r.runDocker(ctx, []string{
		"exec", "--user", WorkerUser, "--workdir", WorktreePath,
		name, "/bin/sh", "-c",
		"mkdir -p \"$HOME/.codex\"; rm -f \"$HOME/.codex/auth.json\"; ln -s \"" + CredentialPath + "/auth.json\" \"$HOME/.codex/auth.json\"",
	}); err != nil {
		return fmt.Errorf("link Codex credentials into the role home: %w", dockerStderrDetail(err))
	}
	return nil
}

// dockerStderrDetail appends the captured Docker standard error to err when the
// adapter recorded any, so a failed invocation reports its reason instead of an
// exit code alone. It never adds command arguments or process input.
func dockerStderrDetail(err error) error {
	var commandErr *dockerCommandError
	if errors.As(err, &commandErr) {
		if detail := strings.TrimSpace(commandErr.Stderr); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
	}
	return err
}

// NativeSessionIDs discovers Codex sessions from persisted role-home files,
// never by scraping terminal output. An empty result means the harness has not
// persisted a session file yet.
func (r *DockerRuntime) NativeSessionIDs(ctx context.Context, request NativeSessionRequest) ([]string, error) {
	if err := validateRunID(request.RunID); err != nil {
		return nil, err
	}
	if request.Harness != "codex" {
		return nil, fmt.Errorf("native session lookup does not support harness %q", request.Harness)
	}
	result, err := r.RunCommand(ctx, CommandRequest{
		RunID:             request.RunID,
		Command:           `find "$HOME/.codex/sessions" -type f -name 'rollout-*.jsonl' -print 2>/dev/null | sort | sed -n -E 's#^.*/rollout-.*-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$#\1#p'`,
		EnvironmentPolicy: EnvironmentPolicyClean,
		Role:              "coordinator",
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, nil
	}
	trimmed := strings.TrimSpace(result.Stdout)
	if trimmed == "" {
		return nil, nil
	}
	identifiers := make([]string, 0)
	seen := make(map[string]struct{})
	for _, identifier := range strings.Split(trimmed, "\n") {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			continue
		}
		if !validName(identifier) {
			return nil, errors.New("discovered native session identifier is unsafe")
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		identifiers = append(identifiers, identifier)
	}
	return identifiers, nil
}

// NativeSessionID returns the newest currently persisted Codex session for
// compatibility with runtimes that only need one identifier.
func (r *DockerRuntime) NativeSessionID(ctx context.Context, request NativeSessionRequest) (string, error) {
	identifiers, err := r.NativeSessionIDs(ctx, request)
	if err != nil || len(identifiers) == 0 {
		return "", err
	}
	return identifiers[len(identifiers)-1], nil
}

// Attach connects the current process's standard streams to one interactive
// worker harness. It is used by the small host attach helper launched by cmux.
func (r *DockerRuntime) Attach(ctx context.Context, request InteractiveRequest) error {
	if err := validateInteractiveRequest(request); err != nil {
		return err
	}
	inspection, err := r.inspectContainer(ctx, containerName(request.RunID))
	if err != nil {
		return fmt.Errorf("inspect worker before interactive attach: %w", err)
	}
	if !inspection.Running {
		return fmt.Errorf("worker %q is not running", request.RunID)
	}
	args := []string{"exec", "-it", "--workdir", WorktreePath}
	for _, value := range commandEnvironment(CommandRequest{RunID: request.RunID, EnvironmentPolicy: request.EnvironmentPolicy, Role: request.Role, Environment: request.Environment}) {
		args = append(args, "--env", value)
	}
	args = append(args, containerName(request.RunID))
	args = append(args, request.Command...)
	binary := r.DockerBinary
	if strings.TrimSpace(binary) == "" {
		binary = "docker"
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

// Stop stops an existing worker while retaining its writable state and role
// storage for a future Resume call. Stopping an absent or already stopped
// worker is idempotent.
func (r *DockerRuntime) Stop(ctx context.Context, runID string) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	name := containerName(runID)
	inspection, err := r.inspectContainer(ctx, name)
	if err != nil {
		if isContainerNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect worker %q before stop: %w", runID, err)
	}
	if !inspection.Running {
		return nil
	}
	if _, err := r.runDocker(ctx, []string{"stop", name}); err != nil {
		return fmt.Errorf("stop worker %q: %w", runID, err)
	}
	return nil
}

// Inspect reports a run worker's state and returns an empty inspection when
// no corresponding Docker container exists.
func (r *DockerRuntime) Inspect(ctx context.Context, runID string) (Inspection, error) {
	if err := validateRunID(runID); err != nil {
		return Inspection{}, err
	}
	inspection, err := r.inspectContainer(ctx, containerName(runID))
	if err != nil {
		if isContainerNotFound(err) {
			return Inspection{}, nil
		}
		return Inspection{}, fmt.Errorf("inspect worker %q: %w", runID, err)
	}
	return inspection, nil
}

// startContainer starts one existing stopped container by its private name.
func (r *DockerRuntime) startContainer(ctx context.Context, name string) error {
	if _, err := r.runDocker(ctx, []string{"start", name}); err != nil {
		return fmt.Errorf("start existing worker: %w", err)
	}
	return nil
}

// recreateWorker removes a worker whose immutable container configuration
// predates a newly required invocation mount. Its named role and credential
// volumes and bind-mounted worktree remain intact for the replacement.
func (r *DockerRuntime) recreateWorker(ctx context.Context, name string, running bool) error {
	if running {
		if _, err := r.runDocker(ctx, []string{"stop", name}); err != nil {
			return fmt.Errorf("stop worker before recreation: %w", err)
		}
	}
	if _, err := r.runDocker(ctx, []string{"rm", name}); err != nil {
		return fmt.Errorf("remove worker before recreation: %w", err)
	}
	return nil
}

// inspectContainer reads lifecycle, image, and stable mount fields from Docker.
func (r *DockerRuntime) inspectContainer(ctx context.Context, name string) (Inspection, error) {
	result, err := r.runDocker(ctx, []string{
		"container", "inspect",
		"--format", "{{json .}}",
		name,
	})
	if err != nil {
		return Inspection{}, err
	}
	trimmed := strings.TrimSpace(result.Stdout)
	if strings.HasPrefix(trimmed, "{") {
		return parseDockerInspectionJSON([]byte(trimmed))
	}
	fields := strings.SplitN(trimmed, "\t", 3)
	if len(fields) < 2 || len(fields) > 3 {
		return Inspection{}, fmt.Errorf("docker inspect returned malformed worker state %q", result.Stdout)
	}
	running, err := strconv.ParseBool(fields[0])
	if err != nil {
		return Inspection{}, fmt.Errorf("docker inspect returned invalid running state %q: %w", fields[0], err)
	}
	image := fields[1]
	var mounts []ContainerMount
	mountDestinations := make(map[string]struct{})
	if len(fields) == 3 && strings.TrimSpace(fields[2]) != "" {
		for _, line := range strings.Split(strings.TrimSpace(fields[2]), "\n") {
			mountFields := strings.Split(line, "\t")
			if len(mountFields) != 3 {
				return Inspection{}, fmt.Errorf("docker inspect returned malformed mount record %q", line)
			}
			readWrite, err := strconv.ParseBool(mountFields[2])
			if err != nil {
				return Inspection{}, fmt.Errorf("docker inspect returned invalid mount read-write flag %q in record %q: %w", mountFields[2], line, err)
			}
			destination := mountFields[1]
			mounts = append(mounts, ContainerMount{
				Source:      mountFields[0],
				Destination: destination,
				ReadOnly:    !readWrite,
			})
			mountDestinations[destination] = struct{}{}
		}
	}
	return Inspection{Exists: true, Running: running, Image: image, Mounts: mounts, mountDestinations: mountDestinations}, nil
}

// dockerInspection is the private subset of Docker's inspect document needed
// to detect stale worker configuration without exposing host paths through the
// WorkerRuntime seam.
type dockerInspection struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// parseDockerInspectionJSON converts Docker's structured inspection response
// into the adapter-private lifecycle and mount representation.
func parseDockerInspectionJSON(data []byte) (Inspection, error) {
	var document dockerInspection
	if err := json.Unmarshal(data, &document); err != nil {
		return Inspection{}, fmt.Errorf("docker inspect returned malformed JSON: %w", err)
	}
	mountIdentities := make(map[string]string, len(document.Mounts))
	mounts := make([]ContainerMount, 0, len(document.Mounts))
	for _, mount := range document.Mounts {
		if strings.TrimSpace(mount.Destination) == "" {
			continue
		}
		identity := dockerMountIdentity(mount.Type, mount.Source, mount.Name)
		if identity != "" {
			mountIdentities[mount.Destination] = identity
		}
		mounts = append(mounts, ContainerMount{
			Source:      mount.Source,
			Destination: mount.Destination,
			ReadOnly:    !mount.RW,
		})
	}
	return Inspection{
		Exists:          true,
		Running:         document.State.Running,
		Image:           document.Config.Image,
		Mounts:          mounts,
		mountIdentities: mountIdentities,
	}, nil
}

// dockerMountIdentity reduces a Docker mount to the stable source identity
// needed for reuse checks while keeping the source private to this adapter.
func dockerMountIdentity(_ string, source, name string) string {
	if strings.TrimSpace(name) != "" {
		return "volume:" + strings.TrimSpace(name)
	}
	if strings.TrimSpace(source) != "" {
		return "bind:" + filepath.Clean(source)
	}
	return ""
}

// runDocker executes one host-side Docker CLI invocation and captures both
// output streams without ever returning the private container name to callers.
func (r *DockerRuntime) runDocker(ctx context.Context, args []string) (dockerCommandResult, error) {
	return r.runDockerWithInput(ctx, args, nil)
}

// runDockerWithInput executes one Docker CLI invocation with optional standard
// input while retaining the adapter's redacted error behavior.
func (r *DockerRuntime) runDockerWithInput(ctx context.Context, args []string, input []byte) (dockerCommandResult, error) {
	binary := r.DockerBinary
	if strings.TrimSpace(binary) == "" {
		binary = "docker"
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := dockerCommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if err == nil {
		return result, nil
	}
	result.ExitCode = 1
	processStarted := false
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		processStarted = true
	}
	return result, &dockerCommandError{
		Args:           append([]string(nil), args...),
		ExitCode:       result.ExitCode,
		ProcessStarted: processStarted,
		Stdout:         result.Stdout,
		Stderr:         result.Stderr,
		Cause:          err,
	}
}

// dockerCommandResult is the internal output of one Docker CLI invocation.
type dockerCommandResult struct {
	// Stdout is the captured standard output.
	Stdout string
	// Stderr is the captured standard error.
	Stderr string
	// ExitCode is zero on success or the Docker process exit code.
	ExitCode int
}

// dockerCommandError preserves Docker output for internal classification.
type dockerCommandError struct {
	// Args are the private Docker command arguments.
	Args []string
	// ExitCode is the failed Docker process exit code.
	ExitCode int
	// ProcessStarted reports whether the Docker executable ran and returned an
	// exit code rather than failing before process creation.
	ProcessStarted bool
	// Stdout is output produced before failure.
	Stdout string
	// Stderr is output produced before failure.
	Stderr string
	// Cause is the underlying process error.
	Cause error
}

// Error reports a redacted Docker command failure without exposing output that
// may contain repository content or credentials.
func (e *dockerCommandError) Error() string {
	return fmt.Sprintf("docker command failed with exit code %d", e.ExitCode)
}

// Unwrap returns the underlying process error.
func (e *dockerCommandError) Unwrap() error { return e.Cause }

// validateStartRequest validates the worker identity, directories, immutable
// image reference, and cache mounts used to start a worker.
func validateStartRequest(request StartRequest) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateDirectory(request.WorktreePath, "worktree path"); err != nil {
		return err
	}
	if err := validateDirectory(request.GitMetadataPath, "Git metadata path"); err != nil {
		return err
	}
	if err := validateImageName(request.Image); err != nil {
		return err
	}
	if !validDigest(request.ImageDigest) {
		return errors.New("worker image digest must be a sha256 digest with 64 lowercase hexadecimal characters")
	}
	if request.InvocationPath != "" {
		if err := validateDirectory(request.InvocationPath, "invocation path"); err != nil {
			return err
		}
	}
	if request.ResultPath != "" {
		if err := validateDirectory(request.ResultPath, "result path"); err != nil {
			return err
		}
	}
	if strings.ContainsAny(request.CredentialStoreID, "\x00\r\n") {
		return errors.New("credential store id contains control characters")
	}
	if request.Role != "" && !validName(request.Role) {
		return fmt.Errorf("worker role %q contains unsafe characters", request.Role)
	}
	seen := make(map[string]struct{}, len(request.Caches))
	for _, cache := range request.Caches {
		if err := validateCacheMount(cache, seen); err != nil {
			return err
		}
	}
	return nil
}

// workerMountsPresent reports whether an existing container already has the
// complete immutable mount set requested by the coordinator. A legacy runtime
// inspection that contains only destinations is accepted as a compatibility
// fallback, but Docker's structured inspection must match each source too.
func workerMountsPresent(request StartRequest, inspection Inspection) bool {
	wanted := expectedWorkerMounts(request)
	if inspection.mountIdentities != nil {
		for destination, identity := range wanted {
			if inspection.mountIdentities[destination] != identity {
				return false
			}
		}
		return true
	}
	for _, destination := range requiredInvocationMounts(request) {
		if _, found := inspection.mountDestinations[destination]; !found {
			return false
		}
	}
	return true
}

// requiredInvocationMounts returns the mounts introduced by the visible
// invocation seam; older adapters cannot safely compare the base mount sources.
func requiredInvocationMounts(request StartRequest) []string {
	required := make([]string, 0, 2)
	if request.InvocationPath != "" {
		required = append(required, InvocationPath)
	}
	if request.ResultPath != "" {
		required = append(required, ResultPath)
	}
	return required
}

// expectedWorkerMounts returns the source identity for every immutable mount
// that must be present before a worker can be reused.
func expectedWorkerMounts(request StartRequest) map[string]string {
	role := request.Role
	if strings.TrimSpace(role) == "" {
		role = "implementation"
	}
	wanted := map[string]string{
		WorktreePath:    "bind:" + filepath.Clean(request.WorktreePath),
		GitMetadataPath: "bind:" + filepath.Clean(request.GitMetadataPath),
		"/home/factory": "volume:" + roleVolumeName(request.RunID, role),
	}
	if request.CredentialStoreID != "" {
		wanted[CredentialPath] = "volume:" + credentialVolumeName(request.RunID, request.CredentialStoreID)
	}
	if request.InvocationPath != "" {
		wanted[InvocationPath] = "bind:" + filepath.Clean(request.InvocationPath)
	}
	if request.ResultPath != "" {
		wanted[ResultPath] = "bind:" + filepath.Clean(request.ResultPath)
	}
	for _, cache := range request.Caches {
		wanted[filepath.Join(CachePath, cache.Name)] = "bind:" + filepath.Clean(cache.HostPath)
	}
	return wanted
}

// validateResumeRequest validates the worker run ID, image name, and immutable
// image digest for a resume request.
func validateResumeRequest(request ResumeRequest) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateImageName(request.Image); err != nil {
		return err
	}
	if !validDigest(request.ImageDigest) {
		return errors.New("worker image digest must be a sha256 digest with 64 lowercase hexadecimal characters")
	}
	return nil
}

// validateCommandRequest validates a worker command request and its explicit
// environment entries.
func validateCommandRequest(request CommandRequest) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if strings.TrimSpace(request.Command) == "" {
		return errors.New("worker command is required")
	}
	if request.EnvironmentPolicy != EnvironmentPolicyClean && request.EnvironmentPolicy != EnvironmentPolicyRole {
		return errors.New("worker environment policy must be clean or role")
	}
	if strings.TrimSpace(request.Role) == "" {
		return errors.New("worker command role is required")
	}
	if !validName(request.Role) {
		return fmt.Errorf("worker command role %q contains unsafe characters", request.Role)
	}
	for name, value := range request.Environment {
		if err := validateEnvironmentEntry(name, value); err != nil {
			return err
		}
	}
	return nil
}

// validateCacheMount validates a cache name, rejects duplicates, records the
// name, and ensures that the host path exists or can be created.
func validateCacheMount(cache CacheMount, seen map[string]struct{}) error {
	if !validName(cache.Name) {
		return fmt.Errorf("cache name %q is invalid", cache.Name)
	}
	if hostHarnessDirectory(cache.HostPath) {
		return fmt.Errorf("cache path %q targets a host harness directory", cache.HostPath)
	}
	if _, exists := seen[cache.Name]; exists {
		return fmt.Errorf("cache name %q is duplicated", cache.Name)
	}
	seen[cache.Name] = struct{}{}
	return ensureCacheDirectory(cache.HostPath)
}

// hostHarnessDirectory identifies the full Codex or Claude state directories
// that may never cross the worker boundary as repository caches.
func hostHarnessDirectory(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if component == ".codex" || component == ".claude" {
			return true
		}
	}
	return false
}

// validateEnvironmentEntry validates an environment variable name and value
// for worker execution. It returns an error for invalid names, forbidden
// characters, disallowed variables, or worker-reserved variables.
func validateEnvironmentEntry(name, value string) error {
	return validateEnvironmentEntryWithProtocol(name, value, false)
}

// validateInteractiveEnvironmentEntry allows only the coordinator's
// invocation identity fields in addition to the ordinary non-secret values.
func validateInteractiveEnvironmentEntry(name, value string) error {
	return validateEnvironmentEntryWithProtocol(name, value, true)
}

// validateEnvironmentEntryWithProtocol validates one environment entry while
// optionally allowing the small set of harness identity values that the
// coordinator must pass to an interactive session.
func validateEnvironmentEntryWithProtocol(name, value string, allowProtocol bool) error {
	if !validEnvironmentName(name) {
		return fmt.Errorf("environment name %q is invalid", name)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("environment value for %q contains a forbidden character", name)
	}
	if forbiddenEnvironmentName(name) {
		return fmt.Errorf("environment variable %q is not allowed in a worker", name)
	}
	if reservedEnvironmentName(name) && (!allowProtocol || !interactiveProtocolEnvironmentName(name)) {
		return fmt.Errorf("environment variable %q is reserved by the worker", name)
	}
	return nil
}

// validateImageName validates that image is a nonempty image name without
// unsafe characters, command-line flags, or mutable tags.
func validateImageName(image string) error {
	trimmed := strings.TrimSpace(image)
	if trimmed == "" {
		return errors.New("worker image is required")
	}
	if trimmed != image || strings.HasPrefix(trimmed, "-") || strings.ContainsAny(trimmed, "\x00\t\r\n,@") {
		return fmt.Errorf("worker image %q contains unsafe characters", image)
	}
	lastComponent := trimmed[strings.LastIndex(trimmed, "/")+1:]
	if strings.Contains(lastComponent, ":") {
		return fmt.Errorf("worker image %q must not include a mutable tag", image)
	}
	return nil
}

// validateMountContract verifies that the existing container's configured
// mounts match the incoming start request's mount requirements.
func validateMountContract(existingMounts []ContainerMount, request StartRequest) error {
	requiredMounts := make(map[string]ContainerMount)
	requiredMounts[WorktreePath] = ContainerMount{
		Source:      request.WorktreePath,
		Destination: WorktreePath,
		ReadOnly:    false,
	}
	requiredMounts[GitMetadataPath] = ContainerMount{
		Source:      request.GitMetadataPath,
		Destination: GitMetadataPath,
		ReadOnly:    true,
	}
	for _, cache := range request.Caches {
		cachePath := filepath.Join(CachePath, cache.Name)
		requiredMounts[cachePath] = ContainerMount{
			Source:      cache.HostPath,
			Destination: cachePath,
			ReadOnly:    cache.ReadOnly,
		}
	}

	existingMountMap := make(map[string]ContainerMount)
	for _, mount := range existingMounts {
		existingMountMap[mount.Destination] = mount
	}

	// The container may also carry mounts this contract does not track (for
	// example the factory-managed credential and role volumes), so only the
	// worktree, Git metadata, and cache mounts declared above are checked.
	for destination, required := range requiredMounts {
		existing, found := existingMountMap[destination]
		if !found {
			return fmt.Errorf("container missing mount at %q", destination)
		}
		if filepath.Clean(existing.Source) != filepath.Clean(required.Source) {
			return fmt.Errorf("mount at %q uses host path %q, want %q", destination, existing.Source, required.Source)
		}
		if existing.ReadOnly != required.ReadOnly {
			readOnlyState := "read-write"
			wantState := "read-write"
			if existing.ReadOnly {
				readOnlyState = "read-only"
			}
			if required.ReadOnly {
				wantState = "read-only"
			}
			return fmt.Errorf("mount at %q is %s, want %s", destination, readOnlyState, wantState)
		}
	}

	return nil
}

// prepareWorkerMounts makes every explicit bind mount accessible to the fixed
// non-root WorkerUser without changing ownership or granting world access. It
// returns the non-root host group IDs that Docker must add to the worker.
func prepareWorkerMounts(request StartRequest) ([]int, error) {
	groupIDs := make(map[int]struct{})
	mounts := []struct {
		name     string
		path     string
		readOnly bool
	}{
		{name: "worktree", path: request.WorktreePath},
		{name: "Git metadata", path: request.GitMetadataPath, readOnly: true},
	}
	if request.InvocationPath != "" {
		mounts = append(mounts, struct {
			name     string
			path     string
			readOnly bool
		}{name: "invocation", path: request.InvocationPath, readOnly: true})
	}
	if request.ResultPath != "" {
		mounts = append(mounts, struct {
			name     string
			path     string
			readOnly bool
		}{name: "result", path: request.ResultPath})
	}
	for _, mount := range mounts {
		if err := prepareWorkerMount(mount.path, mount.readOnly, groupIDs); err != nil {
			return nil, fmt.Errorf("prepare %s mount: %w", mount.name, err)
		}
	}
	for _, cache := range request.Caches {
		if err := prepareWorkerMount(cache.HostPath, cache.ReadOnly, groupIDs); err != nil {
			return nil, fmt.Errorf("prepare cache %q mount: %w", cache.Name, err)
		}
	}
	result := make([]int, 0, len(groupIDs))
	for groupID := range groupIDs {
		if groupID == workerPrimaryGroupID {
			continue
		}
		result = append(result, groupID)
	}
	sort.Ints(result)
	return result, nil
}

// prepareWorkerMount recursively applies the permission strategy used by all
// worker bind mounts. It records each entry's host group for Docker's
// supplemental groups. Symlinks are left untouched so permission preparation
// never follows a link outside the declared mount.
func prepareWorkerMount(path string, readOnly bool, groupIDs map[int]struct{}) error {
	return filepath.WalkDir(path, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported mount entry %q", currentPath)
		}
		groupID, err := hostGroupID(info, currentPath)
		if err != nil {
			return err
		}
		groupIDs[groupID] = struct{}{}
		wanted := workerMountPermissions(info.Mode(), readOnly)
		if info.Mode().Perm() == wanted {
			return nil
		}
		return os.Chmod(currentPath, wanted)
	})
}

// hostGroupID returns the non-root group ID that owns a host mount entry. The
// worker receives this group as a supplemental Docker group so its access is
// limited to the mount's existing host group rather than to every host user.
func hostGroupID(info os.FileInfo, path string) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, fmt.Errorf("cannot determine host group for mount entry %q", path)
	}
	groupID := int(stat.Gid)
	if groupID <= 0 {
		return 0, fmt.Errorf("mount entry %q has unsupported host group id %d", path, groupID)
	}
	return groupID, nil
}

// workerMountPermissions returns owner-plus-group permissions for one worker
// mount entry while preserving executable intent on regular files. Other-user
// permissions are removed so worker access always uses an explicit group.
func workerMountPermissions(mode os.FileMode, readOnly bool) os.FileMode {
	permissions := mode.Perm() & 0o700
	if mode.IsDir() {
		if readOnly {
			return permissions | 0o050
		}
		return permissions | 0o070
	}
	if readOnly {
		permissions |= 0o040
		if mode.Perm()&0o111 != 0 {
			permissions |= 0o010
		}
		return permissions
	}
	permissions |= 0o060
	if mode.Perm()&0o111 != 0 {
		permissions |= 0o010
	}
	return permissions
}

// validateDirectory verifies that path is an absolute, existing directory safe
// for use as a Docker bind mount.
func validateDirectory(path, field string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be absolute", field)
	}
	if strings.ContainsAny(path, "\x00\r\n,") {
		return fmt.Errorf("%s contains a character unsafe for a Docker bind mount", field)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", field, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", field, path)
	}
	return nil
}

// ensureCacheDirectory ensures that path is an absolute, Docker-safe directory,
// creating it when necessary.
func ensureCacheDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("cache path must be absolute")
	}
	if strings.ContainsAny(path, "\x00\r\n,") {
		return errors.New("cache path contains a character unsafe for a Docker bind mount")
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create cache path %q: %w", path, err)
		}
		info, err = os.Stat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect cache path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache path %q is not a directory", path)
	}
	return nil
}

// validateRunID validates that a worker run ID is nonempty and contains only
// safe characters.
func validateRunID(runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("worker run id is required")
	}
	if !validName(runID) {
		return fmt.Errorf("worker run id %q contains unsafe characters", runID)
	}
	return nil
}

// validName reports whether value contains at least one permitted character and
// is not "." or "..".
func validName(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

// validEnvironmentName determines whether name is a valid POSIX environment variable name.
func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if index == 0 && !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_') {
			return false
		}
		if !((character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_') {
			return false
		}
	}
	return true
}

// forbiddenEnvironmentName reports whether an environment variable name
// contains a credential that must not be passed to a worker. Names are matched
// case-insensitively.
func forbiddenEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SSH_AUTH_SOCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "ANTHROPIC_API_KEY", "CODEX_API_KEY":
		return true
	default:
		return strings.HasSuffix(strings.ToUpper(name), "_TOKEN") || strings.HasSuffix(strings.ToUpper(name), "_API_KEY")
	}
}

// reservedEnvironmentName reports whether name is reserved for worker identity,
// stable paths, Git configuration, or clean-environment controls.
func reservedEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "HOME", "PATH", "TERM", "LANG", "LC_ALL", "FACTORY_RUN_ID", "FACTORY_ROLE", "FACTORY_INVOCATION_ID", "FACTORY_HARNESS", "FACTORY_STAGE", "FACTORY_MODEL", "FACTORY_REASONING_EFFORT", "FACTORY_INVOCATION_DIR", "FACTORY_RESULT_DIR", "FACTORY_REPORT_COMMAND", "GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS":
		return true
	default:
		return false
	}
}

// interactiveProtocolEnvironmentName identifies values supplied by the
// coordinator to a visible harness rather than arbitrary worker commands.
func interactiveProtocolEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "FACTORY_INVOCATION_ID", "FACTORY_HARNESS", "FACTORY_STAGE", "FACTORY_MODEL", "FACTORY_REASONING_EFFORT":
		return true
	default:
		return false
	}
}

// validDigest accepts the lowercase SHA-256 form required for image pinning.
func validDigest(value string) bool {
	if len(value) != len(workerImageDigestPrefix)+64 || !strings.HasPrefix(value, workerImageDigestPrefix) {
		return false
	}
	for _, character := range strings.TrimPrefix(value, workerImageDigestPrefix) {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// imageReference combines an image name and its immutable digest.
func imageReference(image, digest string) string {
	return strings.TrimSpace(image) + "@" + strings.TrimSpace(digest)
}

// bindMount formats a Docker bind mount using the stable destination paths.
func bindMount(hostPath, destination string, readOnly bool) string {
	mount := "type=bind,src=" + hostPath + ",dst=" + destination
	if readOnly {
		mount += ",readonly"
	}
	return mount
}

// baselineEnvironment builds the fixed environment used for worker commands,
// including the run identity, stable paths, locale, executable path, and Git
// isolation settings.
func baselineEnvironment(runID string) []string {
	return []string{
		"HOME=/home/factory",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"FACTORY_RUN_ID=" + runID,
		"GIT_DIR=" + GitMetadataPath,
		"GIT_WORK_TREE=" + WorktreePath,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=remote.origin.url",
		"GIT_CONFIG_VALUE_0=disabled://factory",
		"GIT_CONFIG_KEY_1=remote.origin.pushurl",
		"GIT_CONFIG_VALUE_1=disabled://factory",
		"GIT_CONFIG_KEY_2=credential.helper",
		"GIT_CONFIG_VALUE_2=",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"FACTORY_INVOCATION_DIR=" + InvocationPath,
		"FACTORY_RESULT_DIR=" + ResultPath,
		"FACTORY_REPORT_COMMAND=/usr/local/bin/factory-report",
	}
}

// commandEnvironment creates sorted, deterministic environment entries for a
// command. Role identity is available only under the role policy, while both
// policies use the same exact entries for Docker --env and env -i.
func commandEnvironment(request CommandRequest) []string {
	entries := baselineEnvironment(request.RunID)
	if request.EnvironmentPolicy == EnvironmentPolicyRole {
		entries = append(entries, "FACTORY_ROLE="+request.Role)
	}
	entries = append(entries, explicitEnvironment(request.Environment)...)
	return entries
}

// explicitEnvironment returns sorted caller-supplied entries without adding
// worker baseline or role identity values that the adapter owns itself.
func explicitEnvironment(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, key+"="+environment[key])
	}
	return entries
}

// containerName returns the private Docker container name derived from a worker
// run ID.
func containerName(runID string) string {
	digest := sha256.Sum256([]byte(runID))
	return "factory-worker-" + hex.EncodeToString(digest[:])[:24]
}

// roleVolumeName derives a private Docker volume name from a run and role
// without making the volume identity part of any portable workflow seam.
func roleVolumeName(runID, role string) string {
	digest := sha256.Sum256([]byte(runID + "\x00" + role))
	return "factory-role-" + strings.ToLower(role) + "-" + hex.EncodeToString(digest[:])[:16]
}

// credentialVolumeName derives a persistent factory-managed credential volume
// without exposing its coordinator-side key or the host auth path to Docker.
func credentialVolumeName(runID, storeID string) string {
	if strings.TrimSpace(storeID) == "" {
		storeID = runID
	}
	digest := sha256.Sum256([]byte(storeID))
	return "factory-auth-codex-" + hex.EncodeToString(digest[:])[:24]
}

// validateInteractiveRequest validates a harness command and its explicit
// environment before it is handed to a terminal or Docker adapter. Arguments
// may contain newlines because immutable harness prompts are passed as opaque
// argv values; the executable itself and every argument still reject bytes
// that could terminate or rewrite the transport record.
func validateInteractiveRequest(request InteractiveRequest) error {
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return errors.New("interactive harness command is required")
	}
	if request.EnvironmentPolicy != EnvironmentPolicyClean && request.EnvironmentPolicy != EnvironmentPolicyRole {
		return errors.New("interactive environment policy must be clean or role")
	}
	if strings.TrimSpace(request.Role) == "" || !validName(request.Role) {
		return errors.New("interactive harness role is required and must be safe")
	}
	for index, argument := range request.Command {
		if strings.ContainsAny(argument, "\x00\r") || index == 0 && strings.ContainsRune(argument, '\n') {
			return errors.New("interactive harness command contains control characters")
		}
	}
	for name, value := range request.Environment {
		if err := validateInteractiveEnvironmentEntry(name, value); err != nil {
			return err
		}
	}
	return nil
}

// isContainerNotFound reports whether err indicates that Docker could not find a
// container.
func isContainerNotFound(err error) bool {
	var commandErr *dockerCommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
		return false
	}
	message := strings.ToLower(commandErr.Stderr)
	return strings.Contains(message, "no such object") || strings.Contains(message, "no such container")
}

// isDockerRuntimeFailure identifies Docker command errors that indicate a
// runtime or invocation failure rather than a command exit status.
func isDockerRuntimeFailure(commandErr *dockerCommandError) bool {
	message := strings.ToLower(commandErr.Stderr)
	for _, marker := range []string{
		"error response from daemon",
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"unknown flag",
		"unknown command",
		"requires at least",
		"invalid reference format",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

var _ WorkerRuntime = (*DockerRuntime)(nil)
