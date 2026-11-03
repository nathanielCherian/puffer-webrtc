package simulcast

import "time"

// Selector picks a layer for a viewer's video budget. Layers are indexed
// highest bitrate first. It switches down as soon as the current layer no
// longer fits, and up only after the budget has covered the higher layer
// (with UpMargin headroom) for UpHold, so a noisy estimate doesn't flap.
type Selector struct {
	Bitrates []int // nominal bps per layer, highest first
	UpMargin float64
	UpHold   time.Duration

	cand  int // pending up-switch candidate, -1 = none
	since time.Time
}

// NewSelector returns a selector with the default hysteresis.
func NewSelector(bitrates []int) *Selector {
	return &Selector{Bitrates: bitrates, UpMargin: 1.1, UpHold: time.Second, cand: -1}
}

// fit returns the highest layer whose bitrate*margin fits in budget, or the lowest layer.
func (s *Selector) fit(budget int, margin float64) int {
	for i, b := range s.Bitrates {
		if float64(b)*margin <= float64(budget) {
			return i
		}
	}
	return len(s.Bitrates) - 1
}

// Decide returns the layer to send given the budget (bps for video) and the
// current layer (-1 before the first frame).
func (s *Selector) Decide(now time.Time, budget, cur int) int {
	if cur < 0 {
		s.cand = -1
		return s.fit(budget, 1)
	}
	if down := s.fit(budget, 1); down > cur {
		s.cand = -1
		return down
	}
	up := s.fit(budget, s.UpMargin)
	if up >= cur {
		s.cand = -1
		return cur
	}
	if s.cand < 0 {
		s.since = now
	}
	s.cand = up
	if now.Sub(s.since) >= s.UpHold {
		// Keep answering "up" (without restarting the hold) until the switch
		// happens at the higher layer's next keyframe or the budget drops.
		return up
	}
	return cur
}
