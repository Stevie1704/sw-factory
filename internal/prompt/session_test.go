package prompt_test

import (
	"strings"
	"testing"

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
		{workflow.RolePlanning, workflow.PromptVersionPlanning, "849893b461300fd4cc2f80c8619b08acd237f9151b3c8e4d4803d3cd8aee3b86", "--plan-file", false},
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
