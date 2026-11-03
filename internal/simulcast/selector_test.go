package simulcast

import (
	"testing"
	"time"
)

func TestSelector(t *testing.T) {
	s := NewSelector([]int{2_500_000, 800_000, 250_000})
	t0 := time.Unix(0, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	if got := s.Decide(at(0), 1_000_000, -1); got != 1 {
		t.Fatalf("start with 1M budget = %d, want mid", got)
	}
	if got := s.Decide(at(0), 100_000, -1); got != 2 {
		t.Fatalf("start with tiny budget = %d, want low (lowest layer)", got)
	}
	// Down immediately when the current layer no longer fits.
	if got := s.Decide(at(0), 790_000, 1); got != 2 {
		t.Fatalf("mid at 790k = %d, want low", got)
	}
	// Up needs 1.1x headroom ...
	if got := s.Decide(at(0), 2_600_000, 1); got != 1 {
		t.Fatalf("2.6M < 2.75M: got %d, want to stay on mid", got)
	}
	// ... held for a second.
	if got := s.Decide(at(100), 2_800_000, 1); got != 1 {
		t.Fatalf("up before hold: got %d", got)
	}
	if got := s.Decide(at(900), 2_800_000, 1); got != 1 {
		t.Fatalf("up before hold: got %d", got)
	}
	if got := s.Decide(at(1100), 2_800_000, 1); got != 0 {
		t.Fatalf("up after hold: got %d, want high", got)
	}
	// The decision sticks while the switch waits for a keyframe.
	if got := s.Decide(at(1200), 2_800_000, 1); got != 0 {
		t.Fatalf("pending up-switch dropped: got %d", got)
	}
	if got := s.Decide(at(1300), 2_800_000, 0); got != 0 {
		t.Fatalf("after switching: got %d", got)
	}
	// A dip resets the hold.
	s.Decide(at(2000), 3_000_000, 2)
	s.Decide(at(2500), 500_000, 2)
	if got := s.Decide(at(3100), 3_000_000, 2); got != 2 {
		t.Fatalf("hold should restart after a dip: got %d", got)
	}
	if got := s.Decide(at(4200), 3_000_000, 2); got != 0 {
		t.Fatalf("after renewed hold: got %d, want high", got)
	}
	// Stay on the lowest layer even when the budget is below it.
	if got := s.Decide(at(5000), 50_000, 2); got != 2 {
		t.Fatalf("below lowest: got %d", got)
	}
}
