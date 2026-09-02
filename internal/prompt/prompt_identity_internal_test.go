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
// current and retained legacy role body against its checked-in SHA-256 value.
func TestAllEmbeddedPromptVersionsHaveStableContentIdentity(t *testing.T) {
	tests := []struct {
		name    string
		role    string
		stage   string
		version string
		sha256  string
	}{
		{name: "implementation v7", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: workflow.PromptVersionImplementation, sha256: "ea182bf98f0d70c3ac0887b0877a5bb770dc1533f5fcd4072abb729513943a3a"},
		{name: "implementation v6", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v6", sha256: "3e43a7434986c73b61c095e1afaedb3f7aa2b0c37779128ec2dd50b91b15c939"},
		{name: "implementation v5", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v5", sha256: "563454d454a86d5fe9b35c0c141430cac254c83097281e1c70b02711731d77a2"},
		{name: "implementation v4", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v4", sha256: "e169b645f4f22d91fa14941765e004d4c9708949502dd35fe146e2696bc78911"},
		{name: "implementation v3", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v3", sha256: "d1e5598640f885fae8c5f3f650255fba7e9b4c07c0cb790bdbd81537e1fe8354"},
		{name: "implementation v2", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v2", sha256: "658c12098f707a3f400197802747e29b7665428bd00e6f3dd1fe4f0b2923a439"},
		{name: "implementation v1", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v1", sha256: "c482b3b566b3a3e6eae9df5c690efa29a2656d070696cf3798abef3365eda769"},
		{name: "test v3", role: workflow.RoleTest, stage: string(store.StageTest), version: workflow.PromptVersionTest, sha256: "041c14a87705590f02de2a622f58c7361477034f7a99593d8e03bd0050167ae5"},
		{name: "test v2", role: workflow.RoleTest, stage: string(store.StageTest), version: "test-v2", sha256: "5a9bcd6604df2c1bcffddb4571561f371c3854f1d7e16fecf24643ea6d971d3f"},
		{name: "architecture v3", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: workflow.PromptVersionArchitecture, sha256: "c789ad14c540e067207ef00fada44c1c6c56dde111aef945e7a2daf6734eac74"},
		{name: "architecture v2", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: "architecture-v2", sha256: "7f5b0571433ef5c718290fce85e32bd921ed3bd0c3c2b38406d002f84e223e70"},
		{name: "architecture v1", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: "architecture-v1", sha256: "03efc454fd338fdb007244f439d92adb963eaca872d3f024df2ab3c0b970a5d2"},
		{name: "specification review v3", role: workflow.RoleSpecificationReview, stage: string(store.StageReview), version: workflow.PromptVersionSpecificationReview, sha256: "ea66e87d7f144bbc12c63ba2a2d6bb40c05aaef7112cb12d7cb626c858f2c204"},
		{name: "specification review v2", role: workflow.RoleSpecificationReview, stage: string(store.StageReview), version: "specification-review-v2", sha256: "9810c426d9d878e8104f135ac89f33e877d40fd23606e540eda6b025fcb799ed"},
		{name: "specification review v1", role: workflow.RoleSpecificationReview, stage: string(store.StageReview), version: "specification-review-v1", sha256: "a5c7d2a64a84758796036ef8636da3118fef31be49eb3d0d5e45e3ed5caf7507"},
		{name: "standards review v4", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: workflow.PromptVersionStandardsReview, sha256: "007370ac417296565ab8c5ae6e009e6a39591c2a0122acb669f348ceaef9bebb"},
		{name: "standards review v3", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: "standards-review-v3", sha256: "92b351c4f73aa779faa2e915ade8519ff41691c840d8ead77e6207094e32020c"},
		{name: "standards review v2", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: "standards-review-v2", sha256: "40a8037692125475a7796da524bbca2f654e89b61ba097d7e124263184bfc05d"},
		{name: "standards review v1", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: "standards-review-v1", sha256: "f7c0eb584bd5fe0d4b72052bffe96da34c3a2d226bbc5b2046172bf286051472"},
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
