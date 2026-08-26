package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

// main runs the factory CLI with operating-system cancellation wired into the
// long-lived coordinator command.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
