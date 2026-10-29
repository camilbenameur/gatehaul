package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireCreatesTheDirectoryAndExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "up.lock")
	release, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire while held: err = %v, want a deadline error", err)
	}

	if err := release(); err != nil {
		t.Fatal(err)
	}
	release2, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, filepath.Join(t.TempDir(), "x.lock")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
