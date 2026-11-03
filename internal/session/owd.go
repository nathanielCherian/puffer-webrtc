package session

import (
	"encoding/binary"
	"math"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"puffer-webrtc-v2/internal/logs"
)

// One-way delay over the "owd" DataChannel, which the browser opens unordered
// and unreliable (maxRetransmits 0) so it behaves like UDP on the same
// ICE/DTLS path as the media. The server sends a beacon every interval with its
// send time; the browser pings, the server echoes with its receive and send
// times, and the browser estimates the clock offset NTP-style from the
// lowest-RTT ping. Both directions are then computed in the browser:
//
//	server->client:  beacon receive time + offset - beacon send time
//	client->server:  ping t2 - (t1 + offset)
//
// Wire format (big-endian; server times are Unix microseconds):
//
//	beacon  [0]=1  [1:5] seq u32  [5:13] tSend i64   [13:] zero padding
//	ping    [0]=2  [1:5] id u32   [5:13] t1 f64 (browser ms)
//	pong    [0]=3  [1:5] id u32   [5:13] t1 f64 (echoed)  [13:21] t2 i64 (ping received)  [21:29] t3 i64 (pong sent)
const (
	owdTypeBeacon = 1
	owdTypePing   = 2
	owdTypePong   = 3

	owdBeaconHeaderLen = 13
	owdPingLen         = 13
	owdPongLen         = 29
	owdMaxSize         = 1100
)

type owdState struct {
	beaconsSent   atomic.Int64
	beaconsDroppd atomic.Int64 // skipped because the channel was backed up
	pings         atomic.Int64
}

func (o *owdState) snapshot() map[string]any {
	return map[string]any{"beaconsSent": o.beaconsSent.Load(), "beaconsDropped": o.beaconsDroppd.Load(), "pingsAnswered": o.pings.Load()}
}

func appendBeacon(b []byte, seq uint32, tSend time.Time, size int) []byte {
	b = append(b, owdTypeBeacon)
	b = binary.BigEndian.AppendUint32(b, seq)
	b = binary.BigEndian.AppendUint64(b, uint64(tSend.UnixMicro()))
	for len(b) < size {
		b = append(b, 0)
	}
	return b
}

// owdBeaconLoop sends a timestamped datagram every interval until the session ends.
func (s *Session) owdBeaconLoop(dc *webrtc.DataChannel) {
	size := s.cfg.OWDSize
	if size < owdBeaconHeaderLen {
		size = owdBeaconHeaderLen
	}
	if size > owdMaxSize {
		size = owdMaxSize
	}
	s.log.Log("owd", "start", map[string]any{"intervalMs": float64(s.cfg.OWDInterval.Microseconds()) / 1000, "beaconBytes": size})
	ticker := time.NewTicker(s.cfg.OWDInterval)
	defer ticker.Stop()
	buf := make([]byte, 0, owdMaxSize)
	var seq uint32
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		// Never queue beacons behind each other: a backed-up SCTP send buffer
		// would turn into fake delay. Skip the tick instead (and count it).
		if dc.BufferedAmount() > 4*uint64(size) {
			s.owd.beaconsDroppd.Add(1)
			continue
		}
		if err := dc.Send(appendBeacon(buf[:0], seq, time.Now(), size)); err != nil {
			return
		}
		s.owd.beaconsSent.Add(1)
		seq++
	}
}

// onOwdMessage answers a ping with the server's receive and send times and
// logs it (owd.jsonl, kind "ping").
func (s *Session) onOwdMessage(dc *webrtc.DataChannel, p []byte) {
	received := time.Now()
	if len(p) < owdPingLen || p[0] != owdTypePing {
		return
	}
	id := binary.BigEndian.Uint32(p[1:5])
	t1 := math.Float64frombits(binary.BigEndian.Uint64(p[5:13]))
	b := make([]byte, 0, owdPongLen)
	b = append(b, owdTypePong)
	b = append(b, p[1:13]...)
	b = binary.BigEndian.AppendUint64(b, uint64(received.UnixMicro()))
	now := time.Now()
	b = binary.BigEndian.AppendUint64(b, uint64(now.UnixMicro()))
	_ = dc.Send(b)
	s.owd.pings.Add(1)
	s.log.LogAt("owd", received, "ping", map[string]any{"id": id, "t1": t1, "t2": logs.Ms(received), "t3": logs.Ms(now)})
}
