package platform

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// ShutdownContext returns a context that ends when the service manager or the
// user asks the process to stop: on SIGTERM or an interrupt, or when parent
// ends. label is the service's label, by which the manager of an OS without
// signals would address the stop request; on unix the manager sends SIGTERM
// and label is not used. stop releases the signal handler.
func ShutdownContext(parent context.Context, label string) (ctx context.Context, stop context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
