package prompt

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

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
		{name: "implementation v3", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: workflow.PromptVersionImplementation, sha256: "d1e5598640f885fae8c5f3f650255fba7e9b4c07c0cb790bdbd81537e1fe8354"},
		{name: "implementation v2", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v2", sha256: "658c12098f707a3f400197802747e29b7665428bd00e6f3dd1fe4f0b2923a439"},
		{name: "implementation v1", role: workflow.RoleImplementation, stage: string(store.StageImplementation), version: "implementation-v1", sha256: "c482b3b566b3a3e6eae9df5c690efa29a2656d070696cf3798abef3365eda769"},
		{name: "test v2", role: workflow.RoleTest, stage: string(store.StageTest), version: workflow.PromptVersionTest, sha256: "5a9bcd6604df2c1bcffddb4571561f371c3854f1d7e16fecf24643ea6d971d3f"},
		{name: "architecture v2", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: workflow.PromptVersionArchitecture, sha256: "7f5b0571433ef5c718290fce85e32bd921ed3bd0c3c2b38406d002f84e223e70"},
		{name: "architecture v1", role: workflow.RoleArchitecture, stage: string(workflow.StageArchitecture), version: "architecture-v1", sha256: "03efc454fd338fdb007244f439d92adb963eaca872d3f024df2ab3c0b970a5d2"},
		{name: "specification review v1", role: workflow.RoleSpecificationReview, stage: string(store.StageReview), version: workflow.PromptVersionSpecificationReview, sha256: "a5c7d2a64a84758796036ef8636da3118fef31be49eb3d0d5e45e3ed5caf7507"},
		{name: "standards review v2", role: workflow.RoleStandardsReview, stage: string(workflow.StageStandardsReview), version: workflow.PromptVersionStandardsReview, sha256: "40a8037692125475a7796da524bbca2f654e89b61ba097d7e124263184bfc05d"},
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
