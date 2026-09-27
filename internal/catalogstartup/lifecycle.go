package catalogstartup

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// StartUsage excludes unrelated Codex usage traffic from a synthetic startup.
func StartUsage(ctx context.Context, d *Dependencies, start func(context.Context)) {
	if d == nil || !d.IsSynthetic() {
		start(ctx)
	}
}

// Stop cancels the catalog worker and joins any activation currently starting it.
func Stop(cancel context.CancelFunc, gate *sync.Once) {
	cancel()
	gate.Do(func() {})
}

// Join gives an admitted write a bounded opportunity to finish after cancellation.
func (r Run) Join(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d Dependencies
	return d.Wait(wait, r.Done)
}

// Finish handles early run exits, including listener errors before shutdown.
// The activation gate must be joined before reading the Run field.
func Finish(ctx context.Context, cancel context.CancelFunc, gate *sync.Once, run *Run, servers ...*http.Server) error {
	Stop(cancel, gate)
	for _, server := range servers {
		if server != nil {
			server.Close()
		}
	}
	return run.Join(ctx)
}
