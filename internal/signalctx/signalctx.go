// Package signalctx builds the interrupt context the katamaran commands run
// under. The first SIGINT or SIGTERM cancels the context; the second is no
// longer caught, so it terminates the process at the kernel's default
// disposition instead of being swallowed by a handler that keeps running.
package signalctx

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Notify returns a context cancelled by SIGINT or SIGTERM. The caller defers
// the returned stop, which restores the default signal disposition.
func Notify() (context.Context, context.CancelFunc) {
	return notify(func() {})
}

// notify is Notify with a hook that runs once the signal handler has been
// released, so tests can wait for the release instead of sleeping.
func notify(released func()) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
		released()
	}()
	return ctx, stop
}
