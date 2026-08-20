// Package terminal contains the portable terminal-orchestration seam and its
// macOS cmux adapter. Workflow code receives opaque handles only.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// WorkspaceID is an opaque terminal workspace handle.
type WorkspaceID string

// SurfaceID is an opaque terminal surface handle.
type SurfaceID string

// Workspace describes a portable terminal workspace without naming its
// adapter-specific identifier format.
type Workspace struct {
	// ID is the opaque workspace handle.
	ID WorkspaceID
	// Name is the operator-facing workspace name.
	Name string
}

// Surface describes a portable interactive terminal surface.
type Surface struct {
	// ID is the opaque surface handle.
	ID SurfaceID
	// WorkspaceID identifies the owning workspace through the portable seam.
	WorkspaceID WorkspaceID
	// Name is the operator-facing surface name.
	Name string
}

// Command describes a command a terminal adapter should launch.
type Command struct {
	// Executable is the host-side executable or helper name.
	Executable string
	// Args are arguments passed without shell interpolation.
	Args []string
	// Environment contains explicit non-secret values for the surface.
	Environment map[string]string
	// WorkingDirectory is an optional host-side working directory.
	WorkingDirectory string
}

// WorkspaceRequest asks the terminal adapter to ensure one named workspace.
type WorkspaceRequest struct {
	// Name is the stable operator-facing workspace name.
	Name string
	// Description explains the workspace's ownership.
	Description string
	// WorkingDirectory is the directory used by terminal surfaces.
	WorkingDirectory string
}

// RunWorkspaceRequest asks the adapter to create the required supervision
// workspace for one run.
type RunWorkspaceRequest struct {
	// RunID identifies the factory run without exposing runtime identifiers.
	RunID string
	// Name is the stable operator-facing workspace name.
	Name string
	// Description explains the run's issue or stage.
	Description string
	// WorkingDirectory is the run worktree used by the surfaces.
	WorkingDirectory string
}

// RunWorkspace is the minimum visible run layout required by issue #6.
type RunWorkspace struct {
	// Workspace is the parent workspace.
	Workspace
	// Status is the coordinator status surface.
	Status Surface
	// Implementation is the visible implementation-agent surface.
	Implementation Surface
	// Checks is the deterministic-check surface.
	Checks Surface
}

// SurfaceRequest asks the adapter to create one additional terminal surface.
type SurfaceRequest struct {
	// WorkspaceID identifies the owning workspace.
	WorkspaceID WorkspaceID
	// Name is the operator-facing surface name.
	Name string
	// Command is the command attached to the surface.
	Command Command
}

// Notification is an operator-visible message routed to one workspace.
type Notification struct {
	// WorkspaceID identifies the workspace receiving the message.
	WorkspaceID WorkspaceID
	// Title is a short notification title.
	Title string
	// Body is a concise observable status message.
	Body string
}

// TerminalRuntime is the portable seam for workspace, surface, input,
// notification, and lifecycle behavior. cmux and macOS identifiers are owned
// by the adapter implementation.
type TerminalRuntime interface {
	// EnsureControlWorkspace creates or returns the coordinator workspace.
	EnsureControlWorkspace(context.Context, WorkspaceRequest) (Workspace, error)
	// EnsureRunWorkspace creates the required status, implementation, and
	// checks surfaces for one active run.
	EnsureRunWorkspace(context.Context, RunWorkspaceRequest) (RunWorkspace, error)
	// CreateSurface creates one command-backed interactive surface.
	CreateSurface(context.Context, SurfaceRequest) (Surface, error)
	// LaunchSurface starts a command in an existing shell-backed surface.
	LaunchSurface(context.Context, SurfaceID, Command) error
	// SendInput types bytes into a visible surface.
	SendInput(context.Context, SurfaceID, []byte) error
	// Notify publishes an operator-visible workspace message.
	Notify(context.Context, Notification) error
	// CloseSurface stops one surface while retaining workspace state.
	CloseSurface(context.Context, SurfaceID) error
	// CloseWorkspace closes one workspace after its run is terminal.
	CloseWorkspace(context.Context, WorkspaceID) error
}

// CommandRunner is the executable seam for the terminal adapter.
type CommandRunner interface {
	// Run executes one terminal-control command with optional standard input.
	Run(context.Context, []string, []byte) ([]byte, error)
}

// CmuxRuntime implements TerminalRuntime with the macOS cmux command line.
// Its fields and methods contain all cmux-specific command vocabulary.
type CmuxRuntime struct {
	// Runner is injectable for contract tests; nil uses cmux from PATH.
	Runner CommandRunner
	// Binary overrides the cmux executable name.
	Binary string
	// SocketPath is passed to cmux as CMUX_SOCKET_PATH when configured by the
	// host registration.
	SocketPath string
	mu         sync.Mutex
	control    Workspace
	runs       map[string]RunWorkspace
}

// NewCmuxRuntime creates a cmux-backed terminal runtime.
func NewCmuxRuntime(runner CommandRunner) *CmuxRuntime {
	return &CmuxRuntime{Runner: runner, runs: make(map[string]RunWorkspace)}
}

// EnsureControlWorkspace creates the coordinator workspace and returns its
// opaque handle.
func (r *CmuxRuntime) EnsureControlWorkspace(ctx context.Context, request WorkspaceRequest) (Workspace, error) {
	if err := validateWorkspaceRequest(request); err != nil {
		return Workspace{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control.ID != "" {
		control := r.control
		return control, nil
	}
	output, err := r.run(ctx, []string{
		"new-workspace",
		"--name", request.Name,
		"--description", request.Description,
		"--cwd", request.WorkingDirectory,
		"--focus", "false",
		"--layout", workspaceLayout(request, nil),
	}, nil)
	if err != nil {
		return Workspace{}, fmt.Errorf("create control workspace: %w", err)
	}
	control, err := decodeWorkspace(output, request.Name)
	if err != nil {
		return Workspace{}, err
	}
	r.control = control
	return control, nil
}

// EnsureRunWorkspace creates one workspace with status, implementation, and
// checks surfaces. The returned handles are stable opaque values.
func (r *CmuxRuntime) EnsureRunWorkspace(ctx context.Context, request RunWorkspaceRequest) (RunWorkspace, error) {
	if strings.TrimSpace(request.RunID) == "" {
		return RunWorkspace{}, errors.New("run workspace run id is required")
	}
	if strings.TrimSpace(request.Name) == "" {
		return RunWorkspace{}, errors.New("run workspace name is required")
	}
	if err := validateWorkspaceRequest(WorkspaceRequest{Name: request.Name, Description: request.Description, WorkingDirectory: request.WorkingDirectory}); err != nil {
		return RunWorkspace{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[request.RunID]; ok {
		return run, nil
	}
	output, err := r.run(ctx, []string{
		"new-workspace",
		"--name", request.Name,
		"--description", request.Description,
		"--cwd", request.WorkingDirectory,
		"--focus", "true",
		"--layout", runWorkspaceLayout(request),
	}, nil)
	if err != nil {
		return RunWorkspace{}, fmt.Errorf("create run workspace: %w", err)
	}
	decoded, err := decodeRunWorkspace(output, request.Name)
	if err != nil {
		return RunWorkspace{}, err
	}
	if r.runs == nil {
		r.runs = make(map[string]RunWorkspace)
	}
	r.runs[request.RunID] = decoded
	return decoded, nil
}

// CreateSurface creates one command-backed surface in an existing workspace.
func (r *CmuxRuntime) CreateSurface(ctx context.Context, request SurfaceRequest) (Surface, error) {
	if strings.TrimSpace(string(request.WorkspaceID)) == "" {
		return Surface{}, errors.New("surface workspace id is required")
	}
	if strings.TrimSpace(request.Name) == "" {
		return Surface{}, errors.New("surface name is required")
	}
	if err := validateCommand(request.Command); err != nil {
		return Surface{}, err
	}
	output, err := r.run(ctx, []string{
		"new-surface",
		"--workspace", string(request.WorkspaceID),
		"--name", request.Name,
		"--command", commandLine(request.Command),
	}, nil)
	if err != nil {
		return Surface{}, fmt.Errorf("create terminal surface: %w", err)
	}
	var response surfaceResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return Surface{}, fmt.Errorf("decode terminal surface: %w", err)
	}
	surface := response.surface(request.Name, request.WorkspaceID)
	if surface.ID == "" {
		return Surface{}, errors.New("terminal adapter returned no surface handle")
	}
	return surface, nil
}

// LaunchSurface starts a command in a surface created as part of a workspace
// layout. The command is sent as one shell-quoted line; terminal output is
// never read to determine whether it started.
func (r *CmuxRuntime) LaunchSurface(ctx context.Context, surfaceID SurfaceID, command Command) error {
	if strings.TrimSpace(string(surfaceID)) == "" {
		return errors.New("surface id is required")
	}
	if err := validateCommand(command); err != nil {
		return err
	}
	if _, err := r.run(ctx, []string{"send-input", "--surface", string(surfaceID), "--input", "-"}, []byte(commandLine(command)+"\n")); err != nil {
		return fmt.Errorf("launch terminal surface: %w", err)
	}
	return nil
}

// SendInput routes exact bytes to a visible surface; screen content is never
// read or interpreted by this adapter.
func (r *CmuxRuntime) SendInput(ctx context.Context, surfaceID SurfaceID, input []byte) error {
	if strings.TrimSpace(string(surfaceID)) == "" {
		return errors.New("surface id is required")
	}
	if len(input) == 0 {
		return errors.New("surface input is required")
	}
	if _, err := r.run(ctx, []string{"send-input", "--surface", string(surfaceID), "--input", "-"}, input); err != nil {
		return fmt.Errorf("send terminal input: %w", err)
	}
	return nil
}

// Notify publishes a concise workspace notification without reading a
// terminal surface.
func (r *CmuxRuntime) Notify(ctx context.Context, notification Notification) error {
	if strings.TrimSpace(string(notification.WorkspaceID)) == "" {
		return errors.New("notification workspace id is required")
	}
	if strings.TrimSpace(notification.Title) == "" || strings.ContainsAny(notification.Title+notification.Body, "\x00\r\n") {
		return errors.New("notification title and body must be nonempty single lines")
	}
	if _, err := r.run(ctx, []string{"notify", "--workspace", string(notification.WorkspaceID), "--title", notification.Title, "--body", notification.Body}, nil); err != nil {
		return fmt.Errorf("notify terminal workspace: %w", err)
	}
	return nil
}

// CloseSurface closes a surface through the adapter lifecycle command.
func (r *CmuxRuntime) CloseSurface(ctx context.Context, surfaceID SurfaceID) error {
	if strings.TrimSpace(string(surfaceID)) == "" {
		return errors.New("surface id is required")
	}
	if _, err := r.run(ctx, []string{"close-surface", "--surface", string(surfaceID)}, nil); err != nil {
		return fmt.Errorf("close terminal surface: %w", err)
	}
	return nil
}

// CloseWorkspace closes a workspace through the adapter lifecycle command.
func (r *CmuxRuntime) CloseWorkspace(ctx context.Context, workspaceID WorkspaceID) error {
	if strings.TrimSpace(string(workspaceID)) == "" {
		return errors.New("workspace id is required")
	}
	if _, err := r.run(ctx, []string{"close-workspace", "--workspace", string(workspaceID)}, nil); err != nil {
		return fmt.Errorf("close terminal workspace: %w", err)
	}
	return nil
}

// run executes one cmux command through the injected or production runner.
func (r *CmuxRuntime) run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	if r.Runner != nil {
		return r.Runner.Run(ctx, args, input)
	}
	binary := r.Binary
	if strings.TrimSpace(binary) == "" {
		binary = "cmux"
	}
	command := exec.CommandContext(ctx, binary, args...)
	if strings.TrimSpace(r.SocketPath) != "" {
		command.Env = append(os.Environ(), "CMUX_SOCKET_PATH="+r.SocketPath)
	}
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("terminal command failed: %s", message)
	}
	return stdout.Bytes(), nil
}

// validateWorkspaceRequest validates the portable workspace inputs.
func validateWorkspaceRequest(request WorkspaceRequest) error {
	if strings.TrimSpace(request.Name) == "" {
		return errors.New("workspace name is required")
	}
	if strings.ContainsAny(request.Name+request.Description, "\x00\r\n") {
		return errors.New("workspace fields must be single lines")
	}
	if strings.TrimSpace(request.WorkingDirectory) == "" {
		return errors.New("workspace working directory is required")
	}
	return nil
}

// validateCommand validates the command before it crosses into a terminal
// adapter, preventing accidental shell-control characters in executable data.
func validateCommand(command Command) error {
	if strings.TrimSpace(command.Executable) == "" {
		return errors.New("surface command executable is required")
	}
	if strings.ContainsAny(command.Executable, "\x00\r\n") {
		return errors.New("surface command executable contains control characters")
	}
	for _, argument := range command.Args {
		if strings.ContainsAny(argument, "\x00\r\n") {
			return errors.New("surface command argument contains control characters")
		}
	}
	for key, value := range command.Environment {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key+value, "\x00\r\n") {
			return errors.New("surface command environment is invalid")
		}
	}
	return nil
}

// workspaceLayout returns a JSON layout containing one coordinator shell.
func workspaceLayout(request WorkspaceRequest, command *Command) string {
	value := "factory status"
	if command != nil {
		value = commandLine(*command)
	}
	layout, _ := json.Marshal(map[string]any{
		"direction": "horizontal",
		"children":  []map[string]any{{"pane": map[string]any{"surfaces": []map[string]string{{"type": "terminal", "command": value}}}}},
	})
	return string(layout)
}

// runWorkspaceLayout returns the required three-surface run layout.
func runWorkspaceLayout(request RunWorkspaceRequest) string {
	layout, _ := json.Marshal(map[string]any{
		"direction": "vertical",
		"children": []map[string]any{
			{"pane": map[string]any{"surfaces": []map[string]string{{"type": "terminal", "name": "status", "command": "factory status"}}}},
			{"pane": map[string]any{"surfaces": []map[string]string{{"type": "terminal", "name": "implementation", "command": "sh"}}}},
			{"pane": map[string]any{"surfaces": []map[string]string{{"type": "terminal", "name": "checks", "command": "factory status"}}}},
		},
	})
	return string(layout)
}

// commandLine produces a conservative shell command for cmux's layout JSON.
func commandLine(command Command) string {
	parts := []string{shellQuote(command.Executable)}
	for _, argument := range command.Args {
		parts = append(parts, shellQuote(argument))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes one argument for the adapter's layout command string.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// workspaceResponse decodes the small common workspace response shape.
type workspaceResponse struct {
	ID          string            `json:"id"`
	WorkspaceID string            `json:"workspace_id"`
	Name        string            `json:"name"`
	Surfaces    []surfaceResponse `json:"surfaces"`
}

// surfaceResponse decodes one adapter-owned surface handle.
type surfaceResponse struct {
	ID          string `json:"id"`
	SurfaceID   string `json:"surface_id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

// surface returns a portable surface from either supported response id field.
func (response surfaceResponse) surface(defaultName string, workspaceID WorkspaceID) Surface {
	identifier := response.ID
	if identifier == "" {
		identifier = response.SurfaceID
	}
	owner := response.WorkspaceID
	if owner == "" {
		owner = string(workspaceID)
	}
	name := response.Name
	if name == "" {
		name = defaultName
	}
	return Surface{ID: SurfaceID(identifier), WorkspaceID: WorkspaceID(owner), Name: name}
}

// decodeWorkspace decodes a workspace response and rejects empty handles.
func decodeWorkspace(data []byte, defaultName string) (Workspace, error) {
	var response workspaceResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return Workspace{}, fmt.Errorf("decode terminal workspace: %w", err)
	}
	identifier := response.ID
	if identifier == "" {
		identifier = response.WorkspaceID
	}
	if identifier == "" {
		return Workspace{}, errors.New("terminal adapter returned no workspace handle")
	}
	name := response.Name
	if name == "" {
		name = defaultName
	}
	return Workspace{ID: WorkspaceID(identifier), Name: name}, nil
}

// decodeRunWorkspace decodes the required named surfaces from one adapter
// response and fails closed when any surface is missing.
func decodeRunWorkspace(data []byte, defaultName string) (RunWorkspace, error) {
	var response workspaceResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return RunWorkspace{}, fmt.Errorf("decode run workspace: %w", err)
	}
	workspace, err := decodeWorkspace(data, defaultName)
	if err != nil {
		return RunWorkspace{}, err
	}
	run := RunWorkspace{Workspace: workspace}
	for _, candidate := range response.Surfaces {
		surface := candidate.surface(candidate.Name, workspace.ID)
		switch surface.Name {
		case "status":
			run.Status = surface
		case "implementation":
			run.Implementation = surface
		case "checks":
			run.Checks = surface
		}
	}
	if run.Status.ID == "" {
		return RunWorkspace{}, errors.New("run workspace is missing status surface")
	}
	if run.Implementation.ID == "" {
		return RunWorkspace{}, errors.New("run workspace is missing implementation surface")
	}
	if run.Checks.ID == "" {
		return RunWorkspace{}, errors.New("run workspace is missing checks surface")
	}
	return run, nil
}

var _ TerminalRuntime = (*CmuxRuntime)(nil)
