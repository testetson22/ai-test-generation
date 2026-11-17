package testkit

import (
	"testing"
	"time"
)

func TestManualClockAdvancesWithoutSleeping(t *testing.T) {
	start := time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)
	clock := NewManualClock(start)
	if err := clock.Advance(15 * time.Second); err != nil {
		t.Fatalf("advance clock: %v", err)
	}
	if want := start.Add(15 * time.Second); !clock.Now().Equal(want) {
		t.Fatalf("clock = %s, want %s", clock.Now(), want)
	}
	if err := clock.Advance(-time.Second); err != ErrNegativeClockAdvance {
		t.Fatalf("negative advance error = %v, want %v", err, ErrNegativeClockAdvance)
	}
}
