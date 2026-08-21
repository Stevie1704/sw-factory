// Command factory-worker-attach attaches a visible terminal process to a
// worker selected by run identity.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/worker"
)

// main parses only the opaque run identity and explicit environment, then
// delegates private Docker naming and PTY attachment to the worker adapter.
func main() {
	flags := flag.NewFlagSet("factory-worker-attach", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runID := flags.String("run-id", "", "factory run identifier")
	role := flags.String("role", "implementation", "factory workflow role")
	environment := stringList{}
	flags.Var(&environment, "env", "explicit worker environment; may be repeated")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	if flags.NArg() == 0 || strings.TrimSpace(*runID) == "" {
		_, _ = fmt.Fprintln(os.Stderr, "factory-worker-attach requires --run-id and a command after --")
		os.Exit(2)
	}
	command := append([]string(nil), flags.Args()...)
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			_, _ = fmt.Fprintf(os.Stderr, "error: invalid --env value %q\n", entry)
			os.Exit(2)
		}
		values[key] = value
	}
	if strings.TrimSpace(*role) == "" {
		_, _ = fmt.Fprintln(os.Stderr, "error: factory-worker-attach requires a nonempty --role")
		os.Exit(2)
	}
	err := (&worker.DockerRuntime{}).Attach(context.Background(), worker.InteractiveRequest{
		RunID:             *runID,
		Command:           command,
		EnvironmentPolicy: worker.EnvironmentPolicyRole,
		Role:              *role,
		Environment:       values,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// stringList collects repeatable helper environment values.
type stringList []string

// String returns the flag's comma-separated representation.
func (value *stringList) String() string { return strings.Join(*value, ",") }

// Set rejects empty helper environment values.
func (value *stringList) Set(input string) error {
	if strings.TrimSpace(input) == "" || !strings.Contains(input, "=") {
		return errors.New("value must use key=value form")
	}
	*value = append(*value, input)
	return nil
}
