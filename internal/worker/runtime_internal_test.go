package worker

import (
	"testing"
)

const internalWorkerDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDockerInspectionRejectsAStaleInvocationSource verifies an existing
// worker cannot reuse the right destination with the wrong invocation data.
func TestDockerInspectionRejectsAStaleInvocationSource(t *testing.T) {
	request := StartRequest{
		RunID:           "run-inspection",
		WorktreePath:    "/host/worktree",
		GitMetadataPath: "/host/git",
		InvocationPath:  "/host/new-packet",
		ResultPath:      "/host/new-results",
		Image:           "ghcr.io/example/factory-worker",
		ImageDigest:     internalWorkerDigest,
		Role:            "implementation",
	}
	wanted := expectedWorkerMounts(request)
	inspection := Inspection{
		Exists:          true,
		Running:         true,
		Image:           imageReference(request.Image, request.ImageDigest),
		mountIdentities: wanted,
	}
	matchingInvocation := wanted[InvocationPath]
	inspection.mountIdentities[InvocationPath] = "bind:/host/old-packet"
	if workerMountsPresent(request, inspection) {
		t.Fatal("workerMountsPresent() accepted a stale invocation source")
	}
	inspection.mountIdentities[InvocationPath] = matchingInvocation
	if !workerMountsPresent(request, inspection) {
		t.Fatal("workerMountsPresent() rejected matching worker sources")
	}
}

// TestParseDockerInspectionJSONKeepsMountSourcesAdapterPrivate verifies the
// structured Docker response is reduced to source identities in the adapter.
func TestParseDockerInspectionJSONKeepsMountSourcesAdapterPrivate(t *testing.T) {
	inspection, err := parseDockerInspectionJSON([]byte(`{
      "State": {"Running": true},
      "Config": {"Image": "ghcr.io/example/factory-worker@` + internalWorkerDigest + `"},
      "Mounts": [
        {"Type": "bind", "Source": "/host/worktree", "Destination": "/work"},
        {"Type": "volume", "Name": "factory-role-implementation-abc", "Destination": "/home/factory"}
      ]
    }`))
	if err != nil {
		t.Fatalf("parseDockerInspectionJSON() error = %v", err)
	}
	if !inspection.Running || inspection.Image == "" || inspection.mountIdentities[WorktreePath] != "bind:/host/worktree" || inspection.mountIdentities["/home/factory"] != "volume:factory-role-implementation-abc" {
		t.Fatalf("inspection = %#v, want reduced lifecycle and mount identities", inspection)
	}
}
