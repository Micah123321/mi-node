package updateagent

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

type retryState struct {
	Failures int       `json:"failures"`
	Next     time.Time `json:"next_attempt_at"`
}

// Timer invocations use persisted backoff without delaying unresolved local recovery.
func (a *Agent) Run(ctx context.Context, capability, init string, container bool) (result error) {
	path := filepath.Join(a.Engine.Dir, "retry.json")
	var retry retryState
	if err := readJSON(path, &retry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tx, err := a.Engine.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	needsRecovery := err == nil && tx.Started && !tx.Resolved
	if !needsRecovery && time.Now().Before(retry.Next) {
		return nil
	}
	defer func() {
		if result == nil {
			retry = retryState{}
		} else {
			retry.Failures++
			shift := retry.Failures - 1
			if shift > 4 {
				shift = 4
			}
			seconds := 60 * (1 << shift)
			if seconds > 900 {
				seconds = 900
			}
			var api *APIError
			if errors.As(result, &api) && api.RetryAfter > time.Duration(seconds)*time.Second {
				seconds = int(api.RetryAfter.Seconds())
				if seconds > 900 {
					seconds = 900
				}
			}
			retry.Next = time.Now().Add(time.Duration(seconds+rand.IntN(16)) * time.Second)
		}
		if saveErr := AtomicJSON(path, retry); saveErr != nil {
			result = errors.Join(result, saveErr)
		}
	}()
	return a.runAttempt(ctx, capability, init, container)
}
