// Package lock is a cross-process file lock, used to keep two gatehaul
// processes from creating codespaces at the same time.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// retryDelay is how long Acquire waits between two attempts.
const retryDelay = 50 * time.Millisecond

// Acquire takes the exclusive lock on path, creating the file and its parent
// directory when needed, and waits until ctx ends. The returned function
// releases the lock; call it once.
func Acquire(ctx context.Context, path string) (release func() error, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	l := flock.New(path)
	ok, err := l.TryLockContext(ctx, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("locking %s: %w", path, errors.New("lock is held by another process"))
	}
	return l.Unlock, nil
}
