package prompt_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestBuildSessionPinsSessionIdentityAndPacket verifies that a session-role
// prompt names the session, its pinned revision, the read-only context
// revision, and the frozen session packet, never a run, and that it states the
// role's report flag and its lack of tracker authority.
func TestBuildSessionPinsSessionIdentityAndPacket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role        string
		version     string
		sha256      string
		reportFlag  string
		continuance bool
	}{
		{workflow.RoleIdeation, workflow.PromptVersionIdeation, "b2c88fe7a651a61953d3c9f1d0d5ea219cdfa0cd4a5090e4ef0440e19bbc134d", "--ideation-file", false},
		{workflow.RolePlanning, workflow.PromptVersionPlanning, "06aae50573d4ab663f04b5068b34bd55935d383fea75b510ec3993ccd883b20d", "--plan-file", false},
		{workflow.RoleIdeation, workflow.PromptVersionIdeation, "b2c88fe7a651a61953d3c9f1d0d5ea219cdfa0cd4a5090e4ef0440e19bbc134d", "--ideation-file", true},
	}
	for _, test := range tests {
		text, identity, err := prompt.BuildSession(prompt.SessionRequest{
			InvocationID: "sinv-1", SessionID: "session-1", SessionRevision: 6, Role: test.role,
			ContextRevision: "0123456789abcdef0123456789abcdef01234567", Continuation: test.continuance,
		})
		if err != nil {
			t.Fatalf("BuildSession(%s) error = %v", test.role, err)
		}
		if identity.Version != test.version || identity.SHA256 != test.sha256 {
			t.Fatalf("BuildSession(%s) identity = %#v, want version %q", test.role, identity, test.version)
		}
		for _, want := range []string{"session-1", "revision 6", "0123456789abcdef0123456789abcdef01234567", prompt.WorkerSessionPacketPath, test.reportFlag, "no tracker"} {
			if !strings.Contains(text, want) {
				t.Fatalf("BuildSession(%s) prompt lacks %q:\n%s", test.role, want, text)
			}
		}
		if strings.Contains(text, "run worktree") || strings.Contains(strings.ToLower(text), "run id") {
			t.Fatalf("BuildSession(%s) prompt speaks about a run:\n%s", test.role, text)
		}
		if test.continuance && !strings.Contains(text, "continues") {
			t.Fatalf("BuildSession(%s) continuation prompt does not say it continues the session:\n%s", test.role, text)
		}
		current, err := prompt.SessionContentIdentity(test.role)
		if err != nil || current != identity {
			t.Fatalf("SessionContentIdentity(%s) = %#v, %v; want %#v", test.role, current, err, identity)
		}
	}
}

// TestBuildSessionRefusesUnpinnedOrUnknownRequests verifies that a session
// prompt needs a declared session role, a known prompt version, a positive
// pinned revision, and a commit context revision.
func TestBuildSessionRefusesUnpinnedOrUnknownRequests(t *testing.T) {
	t.Parallel()

	valid := prompt.SessionRequest{
		InvocationID: "sinv-1", SessionID: "session-1", SessionRevision: 2, Role: workflow.RoleIdeation,
		ContextRevision: "0123456789abcdef0123456789abcdef01234567",
	}
	tests := map[string]func(*prompt.SessionRequest){
		"run role":          func(request *prompt.SessionRequest) { request.Role = workflow.RoleImplementation },
		"unknown version":   func(request *prompt.SessionRequest) { request.PromptVersion = "ideation-v0" },
		"zero revision":     func(request *prompt.SessionRequest) { request.SessionRevision = 0 },
		"missing session":   func(request *prompt.SessionRequest) { request.SessionID = "" },
		"context not a sha": func(request *prompt.SessionRequest) { request.ContextRevision = "main" },
	}
	for name, mutate := range tests {
		request := valid
		mutate(&request)
		if _, _, err := prompt.BuildSession(request); err == nil {
			t.Fatalf("%s: BuildSession() error = nil, want refusal", name)
		}
	}
}

// TestPlanningPromptStatesTheValidatedPlanContract verifies that the current
// planning prompt names the item kinds and size limits that plan validation
// enforces, keeps the planning craft rules, and that the earlier planning
// version stays buildable for persisted invocations.
func TestPlanningPromptStatesTheValidatedPlanContract(t *testing.T) {
	t.Parallel()

	request := prompt.SessionRequest{
		InvocationID: "sinv-1", SessionID: "session-1", SessionRevision: 3, Role: workflow.RolePlanning,
		ContextRevision: "0123456789abcdef0123456789abcdef01234567",
	}
	text, identity, err := prompt.BuildSession(request)
	if err != nil || identity.Version != "planning-v2" {
		t.Fatalf("BuildSession(planning) identity = %#v, %v; want planning-v2", identity, err)
	}
	for _, want := range []string{
		plangraph.KindTask, plangraph.KindInvestigation, plangraph.KindParent,
		fmt.Sprintf("at most %d items", plangraph.MaxPlanNodes),
		fmt.Sprintf("at most %d acceptance criteria", plangraph.MaxAcceptanceCriteria),
		fmt.Sprintf("at most %d dependencies", plangraph.MaxDependencies),
		fmt.Sprintf("at least %d children", plangraph.MinParentChildren),
		"Prefer one item", "investigation item", "Do not invent a decision", "unresolved question", "Do not add scope",
		"needs_clarification",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("planning prompt lacks %q:\n%s", want, text)
		}
	}
	request.PromptVersion = "planning-v1"
	if _, legacy, err := prompt.BuildSession(request); err != nil || legacy.SHA256 != "849893b461300fd4cc2f80c8619b08acd237f9151b3c8e4d4803d3cd8aee3b86" {
		t.Fatalf("BuildSession(planning-v1) identity = %#v, %v; want the retained v1 body", legacy, err)
	}
}
