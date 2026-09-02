package prompt

import (
	"strings"
	"testing"
)

const (
	// testCraftStart delimits the synthetic craft section used by marker tests.
	testCraftStart = "<!-- craft:start -->"
	// testCraftEnd closes the synthetic craft section used by marker tests.
	testCraftEnd = "<!-- craft:end -->"
)

// TestValidateCraftSectionRejectsMalformedMarkers verifies current role bodies
// fail closed for every malformed craft-marker shape.
func TestValidateCraftSectionRejectsMalformedMarkers(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: "authority only"},
		{name: "duplicate", body: testCraftStart + "one" + testCraftEnd + testCraftStart + "two" + testCraftEnd},
		{name: "nested", body: testCraftStart + testCraftStart + "craft" + testCraftEnd + testCraftEnd},
		{name: "inverted", body: testCraftEnd + "authority" + testCraftStart},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCraftSection(test.body, "test")
			if err == nil || !strings.Contains(err.Error(), "test") {
				t.Fatalf("validateCraftSection() error = %v, want an error naming the role", err)
			}
		})
	}
}

// TestRenderCraftSectionRemovesMarkers verifies craft prose remains available
// to the role while the factory-owned source markers stay out of the prompt.
func TestRenderCraftSectionRemovesMarkers(t *testing.T) {
	body := "authority before\n\n" + testCraftStart + "\ncraft guidance\n" + testCraftEnd + "\n\nauthority after"
	value, err := renderCraftSection(body, "test")
	if err != nil {
		t.Fatalf("renderCraftSection() error = %v", err)
	}
	if strings.Contains(value, testCraftStart) || strings.Contains(value, testCraftEnd) {
		t.Fatalf("renderCraftSection() retained craft markers: %q", value)
	}
	if !strings.Contains(value, "craft guidance") || !strings.Contains(value, "authority before") || !strings.Contains(value, "authority after") {
		t.Fatalf("renderCraftSection() removed prompt content: %q", value)
	}
}

// TestRenderCraftSectionRendersAnEmptySectionAsNothing verifies an empty craft
// section contributes no prompt prose.
func TestRenderCraftSectionRendersAnEmptySectionAsNothing(t *testing.T) {
	body := "authority before\n\n" + testCraftStart + "\n" + testCraftEnd + "\n\nauthority after"
	value, err := renderCraftSection(body, "spec_review")
	if err != nil {
		t.Fatalf("renderCraftSection() error = %v", err)
	}
	if strings.Contains(value, testCraftStart) || strings.Contains(value, testCraftEnd) || strings.Contains(value, "craft") {
		t.Fatalf("renderCraftSection() retained empty craft section: %q", value)
	}
	if value != "authority before\n\nauthority after" {
		t.Fatalf("renderCraftSection() = %q, want authority-only body", value)
	}
}

// TestRenderConditionalSectionSelectsOneRouteArm verifies one existing
// conditional section can render either its implementation or contract arm.
func TestRenderConditionalSectionSelectsOneRouteArm(t *testing.T) {
	body := "authority before\n<!-- route:start -->\nimplementation craft\n<!-- route:else -->\ncontract craft\n<!-- route:end -->\nauthority after"
	for _, test := range []struct {
		name string
		keep bool
		want string
	}{
		{name: "implementation arm", keep: true, want: "authority before\n\nimplementation craft\n\nauthority after"},
		{name: "contract arm", keep: false, want: "authority before\n\ncontract craft\n\nauthority after"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := renderConditionalSection(body, "route", test.keep, "route")
			if err != nil {
				t.Fatalf("renderConditionalSection() error = %v", err)
			}
			if value != test.want {
				t.Fatalf("renderConditionalSection() = %q, want %q", value, test.want)
			}
		})
	}
}
