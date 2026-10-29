package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeSleepAdvances(t *testing.T) {
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	if err := f.Sleep(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := f.Now().Sub(start); got != 90*time.Second {
		t.Fatalf("clock advanced by %v, want 90s", got)
	}
}

func TestSleepHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewFake(time.Time{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fake.Sleep on a cancelled context = %v, want context.Canceled", err)
	}
	if err := (Real{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Real.Sleep on a cancelled context = %v, want context.Canceled", err)
	}
}
