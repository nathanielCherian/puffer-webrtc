package simulcast

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPaceAudio feeds 20 ms pages in 100 ms bursts, then a 300 ms stall followed
// by a catch-up burst, and checks that pages go out evenly, that the stall moves
// the schedule by at most audioLateJump, and that after audioPaceWindow the pacer
// is back on the original schedule with nothing queued.
func TestPaceAudio(t *testing.T) {
	h := &Hub{subs: map[*Subscriber]struct{}{}, audioOut: make(chan audioPage, audioBacklogPages)}
	sub := &Subscriber{Audio: make(chan *AudioSample, 256)}
	h.subs[sub] = struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.paceAudio(ctx)

	const dur = 20 * time.Millisecond
	start := time.Now()
	var mu sync.Mutex
	sent := map[*AudioSample]time.Duration{} // page -> media position
	feed := func(n int) {
		for range n {
			a := &AudioSample{Duration: dur}
			mu.Lock()
			media := time.Duration(len(sent)) * dur
			sent[a] = media
			mu.Unlock()
			h.audioOut <- audioPage{a: a, arrival: time.Now(), media: media}
		}
	}
	go func() {
		// 1 s of 5-page bursts every 100 ms.
		for i := range 10 {
			time.Sleep(time.Until(start.Add(time.Duration(i) * 100 * time.Millisecond)))
			feed(5)
		}
		// Stall 300 ms, catch up in one burst, then 3 s of steady pages.
		time.Sleep(time.Until(start.Add(1300 * time.Millisecond)))
		feed(15) // media 1000-1280 ms
		for i := range 150 {
			time.Sleep(time.Until(start.Add(1300*time.Millisecond + time.Duration(i)*dur)))
			feed(1)
		}
	}()

	var last time.Time
	for i := range 215 {
		var a *AudioSample
		select {
		case a = <-sub.Audio:
		case <-time.After(2 * time.Second):
			t.Fatalf("page %d never sent", i)
		}
		now := time.Now()
		mu.Lock()
		lag := now.Sub(start) - sent[a] // send time vs the schedule set by the first page
		mu.Unlock()
		switch {
		case i > 5 && i < 50: // bursts: smoothed to one page per 20 ms
			if gap := now.Sub(last); gap < 10*time.Millisecond || gap > 30*time.Millisecond {
				t.Errorf("page %d: gap %v, want ~20ms", i, gap)
			}
		case i >= 65 && i < 180: // after the stall: late by at most audioLateJump
			if lag > audioLateJump+10*time.Millisecond {
				t.Errorf("page %d: %v behind schedule, want <= %v", i, lag, audioLateJump)
			}
		case i >= 180: // the stall has left the window: original schedule, no queue
			if lag < -5*time.Millisecond || lag > 15*time.Millisecond {
				t.Errorf("page %d: %v behind schedule, want ~0", i, lag)
			}
			if n := len(h.audioOut); n > 2 {
				t.Errorf("page %d: backlog %d after the stall", i, n)
			}
		}
		last = now
	}
}
