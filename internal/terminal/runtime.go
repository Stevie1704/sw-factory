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

// RunWorkspace contains the visible surfaces currently assigned to one run.
type RunWorkspace struct {
	// Workspace is the parent workspace.
	Workspace
	// Status is reserved for a future live coordinator-status surface.
	Status Surface
	// Implementation is the visible implementation-agent surface.
	Implementation Surface
	// Checks is reserved for a future live deterministic-check surface.
	Checks Surface
}

// WorkspaceInspection is a read-only terminal topology projection used during
// restart reconciliation. It contains only opaque workspace and surface
// identities; screen contents are deliberately absent.
type WorkspaceInspection struct {
	// Exists reports whether the requested workspace is present in the adapter.
	Exists bool
	// WorkspaceID is the observed opaque workspace handle.
	WorkspaceID WorkspaceID
	// Surfaces contains the currently visible surfaces in the workspace.
	Surfaces []Surface
}

// WorkspaceInspector is the optional read-only topology seam used to verify
// persisted cmux handles before a native session is resumed.
type WorkspaceInspector interface {
	InspectWorkspace(context.Context, WorkspaceID) (WorkspaceInspection, error)
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
	// EnsureRunWorkspace creates the visible surfaces enabled for one active run.
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
	// socketPath is passed to cmux as CMUX_SOCKET_PATH when configured by the
	// host registration. It is immutable after construction.
	socketPath string
	mu         sync.Mutex
	control    Workspace
	runs       map[string]RunWorkspace
}

// NewCmuxRuntime creates a cmux-backed terminal runtime. An optional socket
// path selects the cmux instance used by the runtime.
func NewCmuxRuntime(runner CommandRunner, socketPaths ...string) *CmuxRuntime {
	socketPath := ""
	if len(socketPaths) > 0 {
		socketPath = socketPaths[0]
	}
	return &CmuxRuntime{Runner: runner, socketPath: socketPath, runs: make(map[string]RunWorkspace)}
}

// ConfiguredSocketPath returns the cmux socket path selected at construction.
func (r *CmuxRuntime) ConfiguredSocketPath() string {
	return r.socketPath
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
	output, err := r.run(ctx, workspaceCreateArgs(request.Name, request.Description, request.WorkingDirectory, "false", workspaceLayout(request, nil)), nil)
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

// EnsureRunWorkspace creates one workspace with the currently enabled run
// surfaces. The returned handles are stable opaque values.
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
	output, err := r.run(ctx, workspaceCreateArgs(request.Name, request.Description, request.WorkingDirectory, "true", runWorkspaceLayout(request)), nil)
	if err != nil {
		return RunWorkspace{}, fmt.Errorf("create run workspace: %w", err)
	}
	workspace, err := decodeWorkspace(output, request.Name)
	if err != nil {
		return RunWorkspace{}, err
	}
	// Creation reports only the first surface, so the named supervision
	// surfaces are read back from the adapter's own topology.
	surfaces, err := r.workspaceSurfaces(ctx, workspace.ID)
	if err != nil {
		return RunWorkspace{}, err
	}
	decoded, err := runWorkspaceFrom(workspace, surfaces)
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
	// Surface creation carries neither a title nor a command, so the adapter
	// names the surface and starts its command as separate steps.
	output, err := r.run(ctx, []string{
		"new-surface",
		"--workspace", string(request.WorkspaceID),
		"--type", "terminal",
		"--json", "--id-format", "uuids",
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
	if _, err := r.run(ctx, []string{"rename-tab", "--surface", string(surface.ID), "--workspace", string(request.WorkspaceID), request.Name}, nil); err != nil {
		return Surface{}, fmt.Errorf("name terminal surface: %w", err)
	}
	if err := r.LaunchSurface(ctx, surface.ID, request.Command); err != nil {
		return Surface{}, err
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
	if err := r.sendToSurface(ctx, surfaceID, commandLine(command)+"\n"); err != nil {
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
	if err := r.sendToSurface(ctx, surfaceID, string(input)); err != nil {
		return fmt.Errorf("send terminal input: %w", err)
	}
	return nil
}

// sendToSurface routes text to a surface, delivering a trailing newline as a
// separate Enter keystroke. Interactive programs treat text and its newline
// arriving together as a paste and leave the text uncommitted, so the newline
// must be pressed rather than typed.
func (r *CmuxRuntime) sendToSurface(ctx context.Context, surfaceID SurfaceID, text string) error {
	body := strings.TrimSuffix(text, "\n")
	if body != "" {
		if _, err := r.run(ctx, []string{"send", "--surface", string(surfaceID), body}, nil); err != nil {
			return err
		}
	}
	if !strings.HasSuffix(text, "\n") {
		return nil
	}
	if _, err := r.run(ctx, []string{"send-key", "--surface", string(surfaceID), "Enter"}, nil); err != nil {
		return err
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
	// Surface lookup is scoped to a workspace, so the owning workspace is
	// resolved before the surface is closed.
	workspaceID, err := r.surfaceWorkspace(ctx, surfaceID)
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, []string{"close-surface", "--surface", string(surfaceID), "--workspace", string(workspaceID)}, nil); err != nil {
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

// InspectWorkspace reads one workspace and its surface identities without
// mutating cmux or interpreting terminal output.
func (r *CmuxRuntime) InspectWorkspace(ctx context.Context, workspaceID WorkspaceID) (WorkspaceInspection, error) {
	if strings.TrimSpace(string(workspaceID)) == "" {
		return WorkspaceInspection{}, errors.New("workspace id is required")
	}
	topology, err := r.topology(ctx, []string{"tree", "--workspace", string(workspaceID), "--json", "--id-format", "uuids"})
	if err != nil {
		return WorkspaceInspection{}, err
	}
	for _, window := range topology.Windows {
		for _, workspace := range window.Workspaces {
			if workspace.ID != string(workspaceID) {
				continue
			}
			inspection := WorkspaceInspection{Exists: true, WorkspaceID: workspaceID}
			for _, pane := range workspace.Panes {
				for _, surface := range pane.Surfaces {
					inspection.Surfaces = append(inspection.Surfaces, Surface{ID: SurfaceID(surface.ID), WorkspaceID: workspaceID, Name: surface.Title})
				}
			}
			return inspection, nil
		}
	}
	return WorkspaceInspection{WorkspaceID: workspaceID}, nil
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
	// Deprecation hints are written to the same stream this adapter reports as
	// a failure reason, so silence them and keep stderr to actual errors.
	command.Env = append(os.Environ(), "CMUX_QUIET=1")
	if strings.TrimSpace(r.socketPath) != "" {
		command.Env = append(command.Env, "CMUX_SOCKET_PATH="+r.socketPath)
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
// adapter. Multiline arguments are permitted because the adapter shell-quotes
// each opaque argv value; executable newlines and record-altering bytes remain
// forbidden.
func validateCommand(command Command) error {
	if strings.TrimSpace(command.Executable) == "" {
		return errors.New("surface command executable is required")
	}
	if strings.ContainsAny(command.Executable, "\x00\r\n") {
		return errors.New("surface command executable contains control characters")
	}
	for _, argument := range command.Args {
		if strings.ContainsAny(argument, "\x00\r") {
			return errors.New("surface command argument contains control characters")
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
	layout, _ := json.Marshal(splitLayout("horizontal", []map[string]any{terminalPane("", value)}))
	return string(layout)
}

// passiveRunSurfacesEnabled keeps the dormant status/check layout easy to
// restore once those surfaces display live coordinator and gate output. They
// are disabled while both would only run the same one-shot `factory status`.
const passiveRunSurfacesEnabled = false

// runWorkspaceLayout returns the currently enabled run-surface layout.
func runWorkspaceLayout(request RunWorkspaceRequest) string {
	panes := []map[string]any{terminalPane("implementation", "sh")}
	if passiveRunSurfacesEnabled {
		panes = []map[string]any{
			terminalPane("status", "factory status"),
			panes[0],
			terminalPane("checks", "factory status"),
		}
	}
	layout, _ := json.Marshal(splitLayout("vertical", panes))
	return string(layout)
}

// terminalPane returns one layout pane holding a single terminal surface. An
// empty name leaves the surface unnamed rather than naming it with an empty
// string.
func terminalPane(name, command string) map[string]any {
	surface := map[string]string{"type": "terminal", "command": command}
	if name != "" {
		surface["name"] = name
	}
	return map[string]any{"pane": map[string]any{"surfaces": []map[string]string{surface}}}
}

// splitLayout folds panes into a layout node. A terminal split accepts exactly
// two children, so a single pane becomes a bare pane node and more than two
// panes nest to the right.
func splitLayout(direction string, panes []map[string]any) map[string]any {
	if len(panes) == 1 {
		return panes[0]
	}
	return map[string]any{
		"direction": direction,
		"children":  []map[string]any{panes[0], splitLayout(direction, panes[1:])},
	}
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

// runWorkspaceFrom binds the enabled named surfaces to one workspace and fails
// closed when the adapter omitted a required surface.
func runWorkspaceFrom(workspace Workspace, surfaces map[string]SurfaceID) (RunWorkspace, error) {
	run := RunWorkspace{Workspace: workspace}
	// The required surfaces are checked in a fixed order so that a malformed
	// workspace always reports the same missing surface.
	required := []struct {
		name   string
		target *Surface
	}{
		{name: "implementation", target: &run.Implementation},
	}
	if passiveRunSurfacesEnabled {
		required = []struct {
			name   string
			target *Surface
		}{
			{name: "status", target: &run.Status},
			required[0],
			{name: "checks", target: &run.Checks},
		}
	}
	for _, entry := range required {
		identifier, ok := surfaces[entry.name]
		if !ok || strings.TrimSpace(string(identifier)) == "" {
			return RunWorkspace{}, fmt.Errorf("run workspace is missing %s surface", entry.name)
		}
		*entry.target = Surface{ID: identifier, WorkspaceID: workspace.ID, Name: entry.name}
	}
	return run, nil
}

// workspaceCreateArgs builds the adapter's workspace creation command. The
// canonical verb is required because its legacy alias ignores the structured
// output flags.
func workspaceCreateArgs(name, description, workingDirectory, focus, layout string) []string {
	return []string{
		"workspace", "create",
		"--name", name,
		"--description", description,
		"--cwd", workingDirectory,
		"--focus", focus,
		"--layout", layout,
		"--json", "--id-format", "uuids",
	}
}

// topologyResponse decodes the adapter's window, workspace, pane, and surface
// topology. Only the identity and title of each surface is read.
type topologyResponse struct {
	Windows []struct {
		Workspaces []struct {
			ID    string `json:"id"`
			Panes []struct {
				Surfaces []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"surfaces"`
			} `json:"panes"`
		} `json:"workspaces"`
	} `json:"windows"`
}

// workspaceSurfaces reads the surfaces of one workspace, keyed by their title.
func (r *CmuxRuntime) workspaceSurfaces(ctx context.Context, workspaceID WorkspaceID) (map[string]SurfaceID, error) {
	topology, err := r.topology(ctx, []string{"tree", "--workspace", string(workspaceID), "--json", "--id-format", "uuids"})
	if err != nil {
		return nil, err
	}
	surfaces := make(map[string]SurfaceID)
	for _, window := range topology.Windows {
		for _, workspace := range window.Workspaces {
			if workspace.ID != string(workspaceID) {
				continue
			}
			for _, pane := range workspace.Panes {
				for _, surface := range pane.Surfaces {
					surfaces[surface.Title] = SurfaceID(surface.ID)
				}
			}
		}
	}
	if len(surfaces) == 0 {
		return nil, fmt.Errorf("terminal workspace %q reported no surfaces", workspaceID)
	}
	return surfaces, nil
}

// surfaceWorkspace resolves the workspace owning one surface, because surface
// lifecycle commands are scoped to a workspace.
func (r *CmuxRuntime) surfaceWorkspace(ctx context.Context, surfaceID SurfaceID) (WorkspaceID, error) {
	topology, err := r.topology(ctx, []string{"tree", "--json", "--id-format", "uuids"})
	if err != nil {
		return "", err
	}
	for _, window := range topology.Windows {
		for _, workspace := range window.Workspaces {
			for _, pane := range workspace.Panes {
				for _, surface := range pane.Surfaces {
					if surface.ID == string(surfaceID) {
						return WorkspaceID(workspace.ID), nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("terminal surface %q was not found in any workspace", surfaceID)
}

// topology runs one adapter topology query and decodes its response.
func (r *CmuxRuntime) topology(ctx context.Context, args []string) (topologyResponse, error) {
	output, err := r.run(ctx, args, nil)
	if err != nil {
		return topologyResponse{}, fmt.Errorf("read terminal topology: %w", err)
	}
	var topology topologyResponse
	if err := json.Unmarshal(output, &topology); err != nil {
		return topologyResponse{}, fmt.Errorf("decode terminal topology: %w", err)
	}
	return topology, nil
}

var _ TerminalRuntime = (*CmuxRuntime)(nil)
var _ WorkspaceInspector = (*CmuxRuntime)(nil)
