package worker

import (
	"encoding/base64"
	"testing"
)

// TestHeadlessInspectionCarriesTheHelperStartFailure verifies the helper's
// start-failure state reaches the adapter as its own field, so the harness
// classifies a missing executable without reading process output, and that
// a child printing the start-failure line cannot claim that state.
func TestHeadlessInspectionCarriesTheHelperStartFailure(t *testing.T) {
	marker := base64.StdEncoding.EncodeToString([]byte(HeadlessStartFailedMarker + "\n"))
	yes, no := true, false
	for _, test := range []struct {
		name        string
		state       *bool
		stderr      string
		startFailed bool
	}{
		{name: "helper recorded a start failure", state: &yes, stderr: marker, startFailed: true},
		{name: "child printed the marker", state: &no, stderr: marker, startFailed: false},
		{name: "older helper without the state", stderr: marker, startFailed: true},
		{name: "older helper without the marker", startFailed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := headlessInspectionWire{Status: string(HeadlessStatusExited), ExitCode: HeadlessStartFailedExitCode, Stderr: test.stderr, StartFailed: test.state}.toInspection()
			if err != nil {
				t.Fatalf("toInspection() error = %v", err)
			}
			if inspection.StartFailed != test.startFailed {
				t.Fatalf("StartFailed = %t, want %t", inspection.StartFailed, test.startFailed)
			}
		})
	}
}
