package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/guide"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// guideHint names the guide entry point in every usage and help message.
const guideHint = "Run 'factory guide' for orientation and 'factory guide <topic>' for procedures."

// runGuide prints the overview, or the one topic its single argument names,
// headed by the identity of the installed build.
func runGuide(_ context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("guide", errorsOutput)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		writeError(errorsOutput, errors.New("guide accepts at most one topic"))
		return 2
	}
	name := guide.Overview
	if flags.NArg() == 1 {
		name = flags.Arg(0)
	}
	body, err := guide.Topic(name)
	if err != nil {
		writeError(errorsOutput, fmt.Errorf("%w: available topics: %s; run 'factory guide' for the overview", err, strings.Join(guide.Topics(), ", ")))
		return 2
	}
	if !writeOutput(output, errorsOutput, "> Installed release: %s\n\n%s", version.Current().Text(), body) {
		return 1
	}
	return 0
}

// runVersion reports the identity of the running build as one text line or
// as the schema-versioned JSON document.
func runVersion(_ context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("version", errorsOutput)
	asJSON := flags.Bool("json", false, "write the build identity as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(errorsOutput, errors.New("version does not accept positional arguments"))
		return 2
	}
	identity := version.Current()
	if !*asJSON {
		if !writeOutput(output, errorsOutput, "%s\n", identity.Text()) {
			return 1
		}
		return 0
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeOutput(output, errorsOutput, "%s\n", encoded) {
		return 1
	}
	return 0
}
