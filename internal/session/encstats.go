package session

import (
	"sort"
	"sync"
	"time"
)

// encoderStats accumulates per-frame encoder measurements. window() returns
// rates over the interval since the previous window() call plus cumulative
// counters. (Per-frame detail is in encoder.jsonl; this feeds the snapshots.)
type encoderStats struct {
	mu sync.Mutex

	framesEncoded   int64
	bytesEncoded    int64
	keyframes       int64
	keyframesForced int64
	framesSkipped   int64
	bitrateChanges  int64
	lastFrameBytes  int
	lastKeyframeAt  time.Time
	configuredBps   int
	targetBps       int

	winStart     time.Time
	winFrames    int64
	winBytes     int64
	winEncodeDur []float64
	winSendDur   []float64
	winMaxFrame  int
}

func newEncoderStats() *encoderStats { return &encoderStats{winStart: time.Now()} }

func (s *encoderStats) frame(bytes int, key, forced bool, encodeDur, sendDur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.framesEncoded++
	s.bytesEncoded += int64(bytes)
	s.lastFrameBytes = bytes
	if key {
		s.keyframes++
		s.lastKeyframeAt = time.Now()
		if forced {
			s.keyframesForced++
		}
	}
	s.winFrames++
	s.winBytes += int64(bytes)
	if bytes > s.winMaxFrame {
		s.winMaxFrame = bytes
	}
	s.winEncodeDur = append(s.winEncodeDur, float64(encodeDur.Microseconds())/1000)
	s.winSendDur = append(s.winSendDur, float64(sendDur.Microseconds())/1000)
}

func (s *encoderStats) skipped() {
	s.mu.Lock()
	s.framesSkipped++
	s.mu.Unlock()
}

func (s *encoderStats) bitrate(targetBps, configuredBps int) {
	s.mu.Lock()
	s.bitrateChanges++
	s.targetBps = targetBps
	s.configuredBps = configuredBps
	s.mu.Unlock()
}

func (s *encoderStats) window() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	secs := now.Sub(s.winStart).Seconds()
	if secs <= 0 {
		secs = 1e-9
	}
	out := map[string]any{
		"framesEncoded":     s.framesEncoded,
		"bytesEncoded":      s.bytesEncoded,
		"keyframes":         s.keyframes,
		"keyframesForced":   s.keyframesForced,
		"framesSkipped":     s.framesSkipped,
		"bitrateChanges":    s.bitrateChanges,
		"configuredBitrate": s.configuredBps,
		"ccTargetBitrate":   s.targetBps,
		"lastFrameBytes":    s.lastFrameBytes,
		"windowSec":         secs,
		"outputBitrate":     float64(s.winBytes*8) / secs,
		"outputFps":         float64(s.winFrames) / secs,
		"maxFrameBytes":     s.winMaxFrame,
		"encodeMsMean":      mean(s.winEncodeDur),
		"encodeMsP95":       pct(s.winEncodeDur, 0.95),
		"encodeMsMax":       pct(s.winEncodeDur, 1),
		"writeSampleMsMean": mean(s.winSendDur),
		"writeSampleMsP95":  pct(s.winSendDur, 0.95),
	}
	if !s.lastKeyframeAt.IsZero() {
		out["msSinceLastKeyframe"] = float64(now.Sub(s.lastKeyframeAt).Microseconds()) / 1000
	}
	if s.winFrames > 0 {
		out["avgFrameBytesInWindow"] = float64(s.winBytes) / float64(s.winFrames)
	}
	s.winStart = now
	s.winFrames, s.winBytes, s.winMaxFrame = 0, 0, 0
	s.winEncodeDur = s.winEncodeDur[:0]
	s.winSendDur = s.winSendDur[:0]
	return out
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[int(p*float64(len(c)-1))]
}
