package acceptance

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// RenderMarkdown writes the concise human-readable form of a report.
func RenderMarkdown(report Report) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Factory acceptance report\n\n")
	fmt.Fprintf(&out, "- Release: %s (%s), revision %s, modified %t\n", report.Release.Version, report.Release.Build, report.Release.Revision, report.Release.Modified)
	fmt.Fprintf(&out, "- Platform: %s\n", report.Release.Platform)
	fmt.Fprintf(&out, "- Host harness: %s\n", harnessText(report.HostHarness))
	fmt.Fprintf(&out, "- Worker digest: %s\n", valueOrUnobserved(report.WorkerDigest))
	for _, harness := range report.WorkerHarnesses {
		fmt.Fprintf(&out, "- Worker harness: %s\n", harnessText(&harness))
	}
	fmt.Fprintf(&out, "- Generated: %s\n- Recorded duration: %ds\n\n", report.GeneratedAt.Format(time.RFC3339), report.DurationSeconds)
	fmt.Fprintf(&out, "A deterministic pass uses fake adapters. It is not evidence that an agent can use the release; only a live pass is.\n\n")
	fmt.Fprintf(&out, "| Evidence | pass | fail | blocked | not-run |\n| --- | --- | --- | --- | --- |\n")
	for _, row := range []struct {
		name   string
		counts map[string]int
	}{{EvidenceLive, report.Summary.Live}, {EvidenceDeterministic, report.Summary.Deterministic}} {
		fmt.Fprintf(&out, "| %s | %d | %d | %d | %d |\n", row.name, row.counts[OutcomePass], row.counts[OutcomeFail], row.counts[OutcomeBlocked], row.counts[OutcomeNotRun])
	}
	fmt.Fprintf(&out, "\n## Scenarios\n\n| Scenario | Live | Deterministic | Gaps |\n| --- | --- | --- | --- |\n")
	for _, scenario := range report.Scenarios {
		fmt.Fprintf(&out, "| `%s` | %s | %s | %s |\n", scenario.ID, outcomeText(scenario.Live), outcomeText(scenario.Deterministic), gapsText(scenario.Gaps))
	}
	renderDetails(&out, report)
	return out.Bytes()
}

// renderDetails writes the interventions, workarounds, and usage of every
// result that recorded them.
func renderDetails(out *bytes.Buffer, report Report) {
	fmt.Fprintf(out, "\n## Interventions by cause\n\n")
	if len(report.InterventionsByCause) == 0 {
		fmt.Fprintf(out, "None recorded.\n")
	}
	for _, cause := range slices.Sorted(maps.Keys(report.InterventionsByCause)) {
		fmt.Fprintf(out, "- %s: %d\n", cause, report.InterventionsByCause[cause])
	}
	fmt.Fprintf(out, "\n## Details\n")
	for _, scenario := range report.Scenarios {
		for _, item := range []struct {
			evidence string
			outcome  Outcome
		}{{EvidenceLive, scenario.Live}, {EvidenceDeterministic, scenario.Deterministic}} {
			if item.outcome.Status == OutcomeNotRun && item.outcome.Reason == notRecorded {
				continue
			}
			fmt.Fprintf(out, "\n### `%s` (%s): %s\n\n", scenario.ID, item.evidence, item.outcome.Status)
			if item.outcome.Reason != "" {
				fmt.Fprintf(out, "- Reason: %s\n", item.outcome.Reason)
			}
			if item.outcome.DurationSeconds != nil {
				fmt.Fprintf(out, "- Duration: %ds\n", *item.outcome.DurationSeconds)
			}
			for _, intervention := range item.outcome.Interventions {
				fmt.Fprintf(out, "- Intervention (%s): %s\n", intervention.Cause, intervention.Summary)
			}
			for _, workaround := range item.outcome.Workarounds {
				fmt.Fprintf(out, "- Unsupported workaround (%s): %s\n", workaround.Kind, workaround.Summary)
			}
			if item.outcome.Usage != nil {
				fmt.Fprintf(out, "- Usage: %s\n", usageText(*item.outcome.Usage))
			}
		}
	}
}

// harnessText names an observed harness, or says that none was observed.
func harnessText(harness *Harness) string {
	if harness == nil || harness.Name == "" {
		return "not observed"
	}
	return harness.Name + " " + valueOrUnobserved(harness.Version)
}

// valueOrUnobserved returns the value, or "not observed" when it is empty.
func valueOrUnobserved(value string) string {
	if value == "" {
		return "not observed"
	}
	return value
}

// outcomeText is the table cell of one outcome.
func outcomeText(outcome Outcome) string {
	if outcome.Status == OutcomeNotRun && outcome.Reason == notRecorded {
		return OutcomeNotRun
	}
	if outcome.Reason == "" {
		return outcome.Status
	}
	return outcome.Status + ": " + outcome.Reason
}

// gapsText is the table cell of a scenario's recorded gaps.
func gapsText(gaps []Gap) string {
	if len(gaps) == 0 {
		return "none"
	}
	issues := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		issues = append(issues, fmt.Sprintf("#%d", gap.Issue))
	}
	return strings.Join(issues, ", ")
}

// usageText describes measured usage, or why it was not measured.
func usageText(usage Usage) string {
	if !usage.Measured {
		return "not measured: " + usage.Reason
	}
	var parts []string
	if usage.InputTokens != nil {
		parts = append(parts, fmt.Sprintf("%d input tokens", *usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		parts = append(parts, fmt.Sprintf("%d output tokens", *usage.OutputTokens))
	}
	if usage.CostUSD != nil {
		parts = append(parts, fmt.Sprintf("USD %.2f", *usage.CostUSD))
	}
	return strings.Join(parts, ", ")
}
