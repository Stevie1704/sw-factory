package store

import "testing"

// TestJSONProjectionHelpersReturnMarshalErrors verifies unsupported values are
// reported by both generic projection forms.
func TestJSONProjectionHelpersReturnMarshalErrors(t *testing.T) {
	if _, err := sliceJSON([]func(){func() {}}); err == nil {
		t.Fatal("sliceJSON() error = nil, want marshal failure")
	}
	value := func() {}
	if _, err := nullableJSON(&value); err == nil {
		t.Fatal("nullableJSON() error = nil, want marshal failure")
	}
}
