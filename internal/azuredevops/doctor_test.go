package azuredevops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// Azure DevOps security namespaces and the paths of their permission checks.
const (
	gitPermissionsPath     = "/contoso/_apis/permissions/2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87"
	cssPermissionsPath     = "/contoso/_apis/permissions/83e28ad4-2d72-4ceb-97b0-c7726d5502c3"
	taggingPermissionsPath = "/contoso/_apis/permissions/bb50f182-8e5e-40b8-bc21-e8752a1e7ae2"
)

// allFactoryTags is a project tag list that holds every factory tag.
const allFactoryTags = `{"count":7,"value":[{"name":"backend"},{"name":"agent-ready"},{"name":"agent-running"},{"name":"agent-needs-input"},{"name":"agent-failed"},{"name":"agent-cancelled"},{"name":"agent-complete"}]}`

// readyAzure returns a fake az CLI for an identity with every permission.
func readyAzure(t *testing.T) *fakeAz {
	t.Helper()
	return newFakeAz(t).
		on("GET /contoso/_apis/connectionData", connectionData).
		on("GET "+repositoryPath, `{"id":"repo-id","name":"service","project":{"id":"project-id"}}`).
		on("GET "+projectPath+"/wit/classificationnodes/Areas", `{"identifier":"area-id"}`).
		on("GET "+gitPermissionsPath+"/4", `{"value":[true]}`).
		on("GET "+gitPermissionsPath+"/16", `{"value":[true]}`).
		on("GET "+gitPermissionsPath+"/16384", `{"value":[true]}`).
		on("GET "+cssPermissionsPath+"/48", `{"value":[true]}`).
		on("GET "+projectPath+"/wit/tags", allFactoryTags)
}

// runChecks runs the adapter's startup checks.
func runChecks(az *fakeAz) doctor.Report {
	return doctor.Run(context.Background(), newClient(az).StartupChecks(repository)...)
}

// TestStartupChecksPassForAReadyIdentity verifies the check names, and that
// the permission checks use the repository and area security tokens.
func TestStartupChecksPassForAReadyIdentity(t *testing.T) {
	t.Parallel()

	az := readyAzure(t)
	report := runChecks(az)
	if !report.Ready() || len(report.Results) != 3 {
		t.Fatalf("report = %#v, want three passed checks", report)
	}
	names := []string{report.Results[0].Name, report.Results[1].Name, report.Results[2].Name}
	if strings.Join(names, ",") != "azure devops authentication,azure devops repository,azure devops permissions" {
		t.Fatalf("check names = %v", names)
	}
	if version := az.called("GET /contoso/_apis/connectionData")[0].url.Query().Get("api-version"); version != "7.1-preview.1" {
		t.Fatalf("connectionData api-version = %q, want the preview version the resource requires", version)
	}
	if token := az.called("GET " + gitPermissionsPath + "/4")[0].url.Query().Get("tokens"); token != "repoV2/project-id/repo-id" {
		t.Fatalf("Git token = %q", token)
	}
	if token := az.called("GET " + cssPermissionsPath + "/48")[0].url.Query().Get("tokens"); token != "vstfs:///Classification/Node/area-id" {
		t.Fatalf("area token = %q", token)
	}
}

// TestStartupChecksNameTheMissingPermission verifies that each failure names
// the missing capability and a corrective action.
func TestStartupChecksNameTheMissingPermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		route   string
		problem string
	}{
		{route: "GET " + gitPermissionsPath + "/4", problem: "contribute"},
		{route: "GET " + gitPermissionsPath + "/16", problem: "create branches"},
		{route: "GET " + gitPermissionsPath + "/16384", problem: "pull requests"},
		{route: "GET " + cssPermissionsPath + "/48", problem: "work items"},
	}
	for _, tc := range tests {
		t.Run(tc.problem, func(t *testing.T) {
			t.Parallel()

			az := readyAzure(t)
			az.routes[tc.route] = []string{`{"value":[false]}`}
			result := runChecks(az).Results[2]
			if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, tc.problem) || result.Action == "" {
				t.Fatalf("permissions = %#v, want a failure naming %q", result, tc.problem)
			}
		})
	}
}

// TestStartupChecksReportASignedOutCliAndAMissingRepository verifies the
// authentication and reachability failures.
func TestStartupChecksReportASignedOutCliAndAMissingRepository(t *testing.T) {
	t.Parallel()

	az := readyAzure(t)
	az.routes["GET /contoso/_apis/connectionData"] = []string{"error:Please run 'az login' to setup account."}
	az.routes["GET "+repositoryPath] = []string{"error:TF401019: repository not found"}
	report := runChecks(az)
	if report.Results[0].Status != doctor.StatusFailed || !strings.Contains(report.Results[0].Action, "az login") {
		t.Fatalf("authentication = %#v, want a failure that asks for az login", report.Results[0])
	}
	if report.Results[1].Status != doctor.StatusFailed || !strings.Contains(report.Results[1].Problem, "contoso/Factory Pilot/service") {
		t.Fatalf("repository = %#v, want a failure naming the repository", report.Results[1])
	}
	if report.Results[2].Status != doctor.StatusFailed {
		t.Fatalf("permissions = %#v, want a failure without a readable repository", report.Results[2])
	}
}

// TestStartupChecksRequireTagCreationOnlyForMissingFactoryTags verifies that
// an identity without the Create tag definition permission passes when every
// factory tag exists, and fails when the first claim would have to create one.
// The last case uses the array-wrapped list that the API reference shows.
func TestStartupChecksRequireTagCreationOnlyForMissingFactoryTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		create string
		ok     bool
	}{
		{name: "missing tags without permission", create: `{"value":[false]}`, ok: false},
		{name: "missing tags with permission", create: `{"value":[true]}`, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			az := readyAzure(t)
			az.routes["GET "+projectPath+"/wit/tags"] = []string{`{"count":1,"value":[{"name":"agent-ready"}]}`}
			az.on("GET "+taggingPermissionsPath+"/2", tc.create)
			result := runChecks(az).Results[2]
			if (result.Status == doctor.StatusPassed) != tc.ok {
				t.Fatalf("permissions = %#v, want passed = %t", result, tc.ok)
			}
			if !tc.ok && !strings.Contains(result.Problem, "agent-running") {
				t.Fatalf("problem = %q, want it to name a missing tag", result.Problem)
			}
			if tc.ok {
				if token := az.called("GET " + taggingPermissionsPath + "/2")[0].url.Query().Get("tokens"); token != "/project-id" {
					t.Fatalf("tagging token = %q, want the project token", token)
				}
			}
		})
	}
	az := readyAzure(t)
	az.routes["GET "+projectPath+"/wit/tags"] = []string{"[" + allFactoryTags + "]"}
	if result := runChecks(az).Results[2]; result.Status != doctor.StatusPassed || len(az.called("GET "+taggingPermissionsPath+"/2")) != 0 {
		t.Fatalf("permissions = %#v, want a pass without a tag permission check when every tag exists", result)
	}
}
