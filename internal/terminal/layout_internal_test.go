package terminal

import (
	"encoding/json"
	"strings"
	"testing"
)

// assertLayoutNode checks one decoded layout node against the terminal's node
// rules: a node is either a pane or a split, a split carries exactly two
// children, and a pane carries at least one surface.
func assertLayoutNode(t *testing.T, node map[string]any, path string) {
	t.Helper()
	pane, hasPane := node["pane"]
	_, hasDirection := node["direction"]
	if hasPane && hasDirection {
		t.Fatalf("%s: node carries both a pane and a direction", path)
	}
	if hasPane {
		surfaces, ok := pane.(map[string]any)["surfaces"].([]any)
		if !ok || len(surfaces) == 0 {
			t.Fatalf("%s: pane carries no surface", path)
		}
		return
	}
	if !hasDirection {
		t.Fatalf("%s: node carries neither a pane nor a direction", path)
	}
	children, ok := node["children"].([]any)
	if !ok {
		t.Fatalf("%s: split carries no children", path)
	}
	if len(children) != 2 {
		t.Fatalf("%s: split carries %d children, want exactly 2", path, len(children))
	}
	for index, child := range children {
		assertLayoutNode(t, child.(map[string]any), path+".children["+string(rune('0'+index))+"]")
	}
}

// TestWorkspaceLayoutsUseBinarySplits verifies that both generated layouts
// satisfy the terminal's split arity. A one-surface workspace is represented
// as a bare pane rather than an unnecessary split.
func TestWorkspaceLayoutsUseBinarySplits(t *testing.T) {
	layouts := map[string]string{
		"control": workspaceLayout(WorkspaceRequest{Name: "factory-control", WorkingDirectory: "/tmp"}, nil),
		"run":     runWorkspaceLayout(RunWorkspaceRequest{RunID: "run-1", Name: "run", WorkingDirectory: "/tmp"}),
	}
	for name, layout := range layouts {
		var node map[string]any
		if err := json.Unmarshal([]byte(layout), &node); err != nil {
			t.Fatalf("%s layout is not a JSON object: %v", name, err)
		}
		assertLayoutNode(t, node, name)
	}
}

// TestRunWorkspaceLayoutEnablesOnlyImplementation verifies that passive status
// and checks surfaces remain dormant until they receive useful live output.
func TestRunWorkspaceLayoutEnablesOnlyImplementation(t *testing.T) {
	var node map[string]any
	if err := json.Unmarshal([]byte(runWorkspaceLayout(RunWorkspaceRequest{RunID: "run-1", Name: "run", WorkingDirectory: "/tmp"})), &node); err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	var walk func(map[string]any)
	walk = func(current map[string]any) {
		if pane, ok := current["pane"].(map[string]any); ok {
			for _, surface := range pane["surfaces"].([]any) {
				found[surface.(map[string]any)["name"].(string)] = true
			}
			return
		}
		for _, child := range current["children"].([]any) {
			walk(child.(map[string]any))
		}
	}
	walk(node)
	if len(found) != 1 || !found["implementation"] {
		t.Fatalf("run layout surfaces = %#v, want implementation only", found)
	}
}

// TestCommandLineQuotesMultilineArguments verifies that a prompt remains one
// shell argument when the terminal launches the host attach helper. Embedded
// newlines and apostrophes are data, not shell syntax.
func TestCommandLineQuotesMultilineArguments(t *testing.T) {
	prompt := "line one\nuser's line two"
	command := Command{
		Executable: "factory-worker-attach",
		Args:       []string{"--", "codex", prompt},
	}
	if err := validateCommand(command); err != nil {
		t.Fatalf("validateCommand() error = %v, want opaque multiline argument accepted", err)
	}
	line := commandLine(command)
	if !strings.HasSuffix(line, " 'line one\nuser'\\''s line two'") {
		t.Fatalf("command line = %q, want one shell-quoted multiline prompt", line)
	}
}
