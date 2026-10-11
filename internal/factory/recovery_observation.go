package factory

import (
	"context"
	"errors"
	"os"

	"github.com/Stevie1704/sw-factory/internal/hostcmd"
)

// observedFailure reduces an inspection error to a bounded observation for a
// recovery discrepancy. Adapters can include subprocess output, and with it
// credentials, in their error text, and a discrepancy reaches status output,
// lifecycle reasons, and the status comment. Only the Factory-authored text
// of typed host-command limits is kept; every other error becomes a class.
func observedFailure(err error) string {
	var (
		timeout     *hostcmd.TimeoutError
		outputLimit *hostcmd.OutputLimitError
	)
	switch {
	case err == nil:
		return "no error"
	case errors.As(err, &timeout):
		return timeout.Error()
	case errors.As(err, &outputLimit):
		return outputLimit.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "inspection was interrupted"
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	default:
		return "inspection failed"
	}
}
