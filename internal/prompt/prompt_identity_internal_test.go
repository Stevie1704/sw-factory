package prompt

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestCurrentRoleBodiesDeclareExactlyOneCraftSection verifies every current
// embedded role body carries one well-formed factory-owned craft section.
func TestCurrentRoleBodiesDeclareExactlyOneCraftSection(t *testing.T) {
	for role, filename := range rolePromptFiles {
		t.Run(role, func(t *testing.T) {
			body, err := rolePromptFS.ReadFile(filename)
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", filename, err)
			}
			if strings.Count(string(body), craftStartMarker) != 1 || strings.Count(string(body), craftEndMarker) != 1 {
				t.Fatalf("role body has craft marker counts start=%d end=%d", strings.Count(string(body), craftStartMarker), strings.Count(string(body), craftEndMarker))
			}
			if err := validateCraftSection(string(body), role); err != nil {
				t.Fatalf("validateCraftSection() error = %v", err)
			}
		})
	}
}

// TestImplementationCraftContainsOneRouteSelectedSubBlock verifies the
// implementation-owned TDD block is the only route-selected craft block and
// is nested inside the one implementation craft section.
func TestImplementationCraftContainsOneRouteSelectedSubBlock(t *testing.T) {
	body, err := rolePromptFS.ReadFile(rolePromptFiles[workflow.RoleImplementation])
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	value := string(body)
	routeMarkers := []string{
		"<!-- implementation-owned-tdd:start -->",
		"<!-- implementation-owned-tdd:else -->",
		"<!-- implementation-owned-tdd:end -->",
	}
	containsOneNestedTDDBlock := func(body string) bool {
		for _, marker := range routeMarkers {
			if strings.Count(body, marker) != 1 {
				return false
			}
		}
		craftStart := strings.Index(body, craftStartMarker)
		craftEnd := strings.Index(body, craftEndMarker)
		tddStart := strings.Index(body, routeMarkers[0])
		tddElse := strings.Index(body, routeMarkers[1])
		tddEnd := strings.Index(body, routeMarkers[2])
		return craftStart >= 0 && craftEnd >= 0 && tddStart >= 0 && tddElse >= 0 && tddEnd >= 0 && craftStart < tddStart && tddStart < tddElse && tddElse < tddEnd && tddEnd < craftEnd
	}
	if !containsOneNestedTDDBlock(value) {
		t.Fatalf("implementation-owned TDD block is not nested in craft section:\n%s", value)
	}
	t.Run("rejects duplicate TDD markers", func(t *testing.T) {
		duplicateTDDStart := value + "\n" + routeMarkers[0]
		if containsOneNestedTDDBlock(duplicateTDDStart) {
			t.Fatalf("duplicate TDD marker was accepted:\n%s", duplicateTDDStart)
		}
	})
	if strings.Contains(value, "<!-- implementation-craft-independent-test-handoff:") {
		t.Fatalf("implementation craft contains a second route-selected block:\n%s", value)
	}
}

// TestAllEmbeddedPromptVersionsHaveStableContentIdentity verifies every
// current role body against its checked-in SHA-256 value.
func TestAllEmbeddedPromptVersionsHaveStableContentIdentity(t *testing.T) {
	tests := []struct {
		name    string
		role    string
		stage   string
		version string
		sha256  string
	}{
		{name: "implementation v9", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: workflow.PromptVersionImplementation, sha256: "e0a6f046876022c8095a3bc161127e2b6ec0518ef51bbc57f0a753fb8a0ca998"},
		{name: "test v3", role: workflow.RoleTest, stage: string(store.StageTest), version: workflow.PromptVersionTest, sha256: "041c14a87705590f02de2a622f58c7361477034f7a99593d8e03bd0050167ae5"},
		{name: "architecture v3", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: workflow.PromptVersionArchitecture, sha256: "c789ad14c540e067207ef00fada44c1c6c56dde111aef945e7a2daf6734eac74"},
		{name: "specification review v8", role: workflow.RoleSpecificationReview, stage: string(store.StageReview), version: workflow.PromptVersionSpecificationReview, sha256: "9f07951a1a8ef748f8e4ef01032383b3191376fa5a321d8f460d425d8d7990a8"},
		{name: "standards review v8", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: workflow.PromptVersionStandardsReview, sha256: "b55ea69e303fbbde83931681a7fb2691849030afe43e975e4f57c69f87f731a0"},
	}
	registry := DefaultRegistry()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, err := registry.roleDefinition(test.role, test.stage)
			if err != nil {
				t.Fatalf("roleDefinition() error = %v", err)
			}
			body, identity, err := registry.roleBody(definition, test.version)
			if err != nil {
				t.Fatalf("roleBody() error = %v", err)
			}
			if body == "" || identity.Version != test.version || identity.SHA256 != test.sha256 {
				t.Fatalf("roleBody() identity = %#v, body empty = %t; want version %q and SHA-256 %q", identity, body == "", test.version, test.sha256)
			}
		})
	}
}
