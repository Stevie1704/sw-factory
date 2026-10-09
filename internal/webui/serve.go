package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// loopbackAddress is the only host the UI listens on. The UI has no login, so
// it must not be reachable from another machine.
const loopbackAddress = "127.0.0.1"

// readHeaderTimeout bounds how long a client may take to send request headers.
const readHeaderTimeout = 5 * time.Second

// shutdownTimeout bounds how long a stop waits for open requests to finish.
const shutdownTimeout = 5 * time.Second

// ValidateAddress accepts only host 127.0.0.1 with an explicit numeric port.
// Port 0 is allowed so tests can ask for a free port.
func ValidateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid UI address %q: %w", address, err)
	}
	if host != loopbackAddress {
		return fmt.Errorf("invalid UI address %q: the host must be %s", address, loopbackAddress)
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || strconv.FormatUint(number, 10) != port {
		return fmt.Errorf("invalid UI address %q: the port must be a number from 0 to 65535", address)
	}
	return nil
}

// Serve listens on address, reports the bound URL through ready, and serves
// handler until ctx is cancelled. It refuses an address that ValidateAddress
// refuses. A graceful stop after cancellation returns nil.
func Serve(ctx context.Context, address string, handler http.Handler, ready func(url string)) error {
	if err := ValidateAddress(address); err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	ready("http://" + listener.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serve UI: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("stop UI: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve UI: %w", err)
	}
	return nil
}
