package catalogstartup

import (
	"context"
	"errors"
)

func RequireActive(active bool) error {
	if !active {
		return errors.New("model catalog refresh requires active instance")
	}
	return nil
}

// Collect abandons the current refresh when cancellation is observed between
// provider probes, before the configuration lock is taken for the merge.
func Collect[P any, R any](ctx context.Context, providers []P, probe func(P) (string, R)) (map[string]R, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := make(map[string]R, len(providers))
	for _, p := range providers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, result := probe(p)
		results[name] = result
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// AdmitPersistence is the last cancellation check before a potentially
// multi-file write. Cancellation after this point does not roll back that write.
func AdmitPersistence(ctx context.Context) error {
	return ctx.Err()
}
