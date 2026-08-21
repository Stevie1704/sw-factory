// Command factory-report publishes one structured harness outcome.
package main

import (
	"os"

	"github.com/Stevie1704/sw-factory/internal/reportcli"
)

// main forwards the worker process environment and arguments to the report
// command without reading or printing credential-bearing values.
func main() {
	os.Exit(reportcli.Run(reportcli.Request{Args: os.Args[1:], Output: os.Stdout, ErrorsOutput: os.Stderr}))
}
