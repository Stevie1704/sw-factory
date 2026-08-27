package worker

import (
	"testing"
)

// internalWorkerDigest is the immutable worker image identity used by runtime
// inspection contract tests.
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
	identities := make(map[string]string, len(wanted))
	for destination, identity := range wanted {
		identities[destination] = identity
	}
	inspection := Inspection{
		Exists:          true,
		Running:         true,
		Image:           imageReference(request.Image, request.ImageDigest),
		mountIdentities: identities,
		mountReadOnly:   expectedWorkerMountReadOnly(request),
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
	known, matches := inspection.MountContractStatus(request)
	if !known || !matches {
		t.Fatalf("MountContractStatus() = known %t, matches %t; want true, true", known, matches)
	}
	unknown, matches := (Inspection{Exists: true}).MountContractStatus(request)
	if unknown || matches {
		t.Fatalf("unknown MountContractStatus() = known %t, matches %t; want false, false", unknown, matches)
	}
}

// TestParseDockerInspectionJSONKeepsMountSourcesAdapterPrivate verifies the
// structured Docker response is reduced to source identities in the adapter.
func TestParseDockerInspectionJSONKeepsMountSourcesAdapterPrivate(t *testing.T) {
	inspection, err := parseDockerInspectionJSON([]byte(`{
	      "State": {"Running": true},
	      "Config": {"Image": "ghcr.io/example/factory-worker@` + internalWorkerDigest + `"},
	      "Mounts": [
	        {"Type": "bind", "Source": "/host/worktree", "Destination": "/work", "RW": true},
	        {"Type": "bind", "Source": "/host/git", "Destination": "/git", "RW": false},
	        {"Type": "volume", "Name": "` + roleVolumeName("run-inspection", "implementation") + `", "Destination": "/home/factory", "RW": true}
	      ]
	    }`))
	if err != nil {
		t.Fatalf("parseDockerInspectionJSON() error = %v", err)
	}
	if !inspection.Running || inspection.Image == "" || inspection.mountIdentities[WorktreePath] != "bind:/host/worktree" || inspection.mountIdentities["/home/factory"] != "volume:"+roleVolumeName("run-inspection", "implementation") {
		t.Fatalf("inspection = %#v, want reduced lifecycle and mount identities", inspection)
	}
	if inspection.MountFingerprint != MountContractFingerprint(StartRequest{RunID: "run-inspection", WorktreePath: "/host/worktree", GitMetadataPath: "/host/git", Role: "implementation"}) {
		t.Fatalf("MountFingerprint = %q, want the matching contract fingerprint", inspection.MountFingerprint)
	}
}
