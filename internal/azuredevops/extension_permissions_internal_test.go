package azuredevops

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// areaPermissionRunner permits the root area and denies a configured child
// area, recording the tokens evaluated through the calling-user CLI helper.
type areaPermissionRunner struct {
	tokens []string
	paths  []string
}

// Run returns distinct area identifiers so a root-only permission check
// cannot accidentally pass the child-area denial regression.
func (r *areaPermissionRunner) Run(_ context.Context, args []string) ([]byte, error) {
	call, err := azuredevopstest.ParseExtensionCall(args)
	if err != nil {
		return nil, err
	}
	switch call.Route() {
	case "repos show":
		return []byte(`{"id":"` + azuredevopstest.RepositoryID + `","name":"service","project":{"id":"` + azuredevopstest.ProjectID + `","name":"Factory Pilot"}}`), nil
	case "devops invoke GET wit/classificationNodes":
		path := call.Parameters("--route-parameters")["path"]
		r.paths = append(r.paths, path)
		if path == "Team A" {
			return []byte(`{"identifier":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`), nil
		}
		return []byte(`{"identifier":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`), nil
	case "factory-devops permission":
		token := call.Flag("--token")
		r.tokens = append(r.tokens, token)
		return []byte(fmt.Sprintf(`{"granted":%t}`, token != "vstfs:///Classification/Node/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")), nil
	}
	return nil, fmt.Errorf("unexpected command %s", call.Route())
}

// TestExtensionPlanPermissionsRefusesAChildAreaDenial proves that root
// permission does not authorize creating items in an explicitly denied area.
func TestExtensionPlanPermissionsRefusesAChildAreaDenial(t *testing.T) {
	t.Parallel()
	runner := &areaPermissionRunner{}
	client := &Client{Runner: runner, Route: "devops_extension", Organization: "contoso"}
	repository := tracker.Repository{Owner: "contoso", Project: "Factory Pilot", Name: "service"}
	err := client.extensionPlanPermissions(t.Context(), repository, []tracker.RenderedPlanItem{
		{}, {Fields: map[string]string{"System.AreaPath": `Factory Pilot\Team A`}},
	})
	if kind, _ := tracker.PlanErrorKindOf(err); kind != tracker.PlanErrorPermission || len(runner.tokens) != 2 || len(runner.paths) != 2 || runner.paths[1] != "Team A" {
		t.Fatalf("permission check = %v, tokens %v, paths %v; want child-area denial", err, runner.tokens, runner.paths)
	}
}
