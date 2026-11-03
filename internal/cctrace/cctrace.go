// Package cctrace wraps a Pion bandwidth estimator and records everything that
// goes into it, so a congestion-control decision can be replayed offline.
//
// Pion's send-side GCC has exactly two inputs:
//
//  1. every RTP packet as it leaves the pacer (send time, transport-wide
//     sequence number, size) — recorded as "sent";
//  2. every TWCC (or RFC 8888) feedback packet, with its arrival time —
//     recorded as "feedback", decoded to per-packet receive times.
//
// It also records the outputs: the target bitrate after each feedback and every
// target change ("target"), plus the estimator's internal stats. The "stream"
// and "config" records describe the setup.
package cctrace

import (
	"encoding/base64"
	"encoding/binary"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"puffer-webrtc-v2/internal/probe"
)

const transportCCURI = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"

// Logger receives one record; t is the event time.
type Logger func(kind string, t time.Time, data any)

// Tracer is a cc.BandwidthEstimator that logs its inputs and outputs.
type Tracer struct {
	cc.BandwidthEstimator
	log Logger

	mu      sync.Mutex
	queued  map[uint64]time.Time // ssrc<<16|seq -> time the packet entered the pacer
	highest map[uint32]*seqTracker
	lastTS  map[uint32]uint32 // ssrc -> RTP timestamp of the newest packet queued (set synchronously inside WriteSample)
}

type seqTracker struct {
	started bool
	seq     uint16
}

// New wraps inner. A "target" record is logged for every target change.
func New(inner cc.BandwidthEstimator, log Logger) *Tracer {
	t := &Tracer{BandwidthEstimator: inner, log: log, queued: map[uint64]time.Time{}, highest: map[uint32]*seqTracker{}, lastTS: map[uint32]uint32{}}
	inner.OnTargetBitrateChange(func(bps int) {
		t.log("target", time.Now(), map[string]any{"bitrate": bps})
	})
	return t
}

// OnTargetBitrateChange keeps the trace callback and chains f after it.
func (t *Tracer) OnTargetBitrateChange(f func(int)) {
	t.BandwidthEstimator.OnTargetBitrateChange(func(bps int) {
		t.log("target", time.Now(), map[string]any{"bitrate": bps})
		if f != nil {
			f(bps)
		}
	})
}

// SentRecord is one RTP packet handed to the estimator's send-time bookkeeping.
type SentRecord struct {
	SSRC    uint32  `json:"ssrc"`
	Seq     uint16  `json:"seq"`
	TWCC    *uint16 `json:"twcc"` // transport-wide sequence number (nil if the stream has no extension)
	PT      uint8   `json:"pt"`
	Marker  bool    `json:"m"`
	RTPTime uint32  `json:"ts"`
	Header  int     `json:"hdr"` // marshalled header bytes
	Payload int     `json:"len"` // payload bytes (what GCC counts)
	Padding bool    `json:"pad"`
	Probe   bool    `json:"probe,omitempty"` // a padding probe (simulcast sessions): GCC counted len bytes, the wire carries them as RTP padding
	RTX     bool    `json:"rtx"`             // sequence number not newer than the highest seen: a NACK resend
	QueueMs float64 `json:"qMs"`             // time spent in the pacer queue
}

// AddStream wraps the stream on both sides of the pacer: the outer writer
// records when a packet is queued, the inner one when it is actually sent.
func (t *Tracer) AddStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var extID uint8
	exts := make([]map[string]any, 0, len(info.RTPHeaderExtensions))
	for _, e := range info.RTPHeaderExtensions {
		exts = append(exts, map[string]any{"id": e.ID, "uri": e.URI})
		if e.URI == transportCCURI {
			extID = uint8(e.ID)
		}
	}
	t.log("stream", time.Now(), map[string]any{
		"ssrc": info.SSRC, "payloadType": info.PayloadType, "mimeType": info.MimeType, "clockRate": info.ClockRate,
		"twccExtId": extID, "rtxSsrc": info.SSRCRetransmission, "headerExtensions": exts,
	})
	tracker := &seqTracker{}
	h264 := probe.IsH264(info.MimeType)
	inner := interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		now := time.Now()
		rec := SentRecord{SSRC: h.SSRC, Seq: h.SequenceNumber, PT: h.PayloadType, Marker: h.Marker, RTPTime: h.Timestamp,
			Header: h.MarshalSize(), Payload: len(payload), Padding: h.Padding,
			Probe: h264 && !h.Padding && probe.IsPayload(payload)}
		if extID != 0 {
			if b := h.GetExtension(extID); len(b) >= 2 {
				v := binary.BigEndian.Uint16(b)
				rec.TWCC = &v
			}
		}
		key := uint64(h.SSRC)<<16 | uint64(h.SequenceNumber)
		t.mu.Lock()
		if q, ok := t.queued[key]; ok {
			rec.QueueMs = float64(now.Sub(q).Microseconds()) / 1000
			delete(t.queued, key)
		}
		switch {
		case !tracker.started:
			tracker.started, tracker.seq = true, h.SequenceNumber
		case int16(h.SequenceNumber-tracker.seq) > 0:
			tracker.seq = h.SequenceNumber
		default:
			rec.RTX = true
		}
		t.mu.Unlock()
		t.log("sent", now, rec)
		return writer.Write(h, payload, attrs)
	})
	pacer := t.BandwidthEstimator.AddStream(info, inner)
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		key := uint64(h.SSRC)<<16 | uint64(h.SequenceNumber)
		t.mu.Lock()
		t.queued[key] = time.Now()
		t.lastTS[h.SSRC] = h.Timestamp
		t.mu.Unlock()
		return pacer.Write(h, payload, attrs)
	})
}

// LastTimestamp returns the RTP timestamp of the newest packet queued for
// ssrc. The packetizer assigns timestamps inside WriteSample, which writes all
// of a frame's packets through the outer writer before returning, so calling
// this right after WriteSample yields the timestamp of that frame.
func (t *Tracer) LastTimestamp(ssrc uint32) (uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts, ok := t.lastTS[ssrc]
	return ts, ok
}

// WriteRTCP logs every congestion-control feedback packet (decoded and raw),
// hands the batch to the estimator, and records the target it settled on.
func (t *Tracer) WriteRTCP(pkts []rtcp.Packet, attrs interceptor.Attributes) error {
	now := time.Now()
	var fbs []any
	for _, p := range pkts {
		switch fb := p.(type) {
		case *rtcp.TransportLayerCC:
			fbs = append(fbs, DecodeTWCC(fb))
		case *rtcp.CCFeedbackReport:
			fbs = append(fbs, decodeCCFB(fb))
		}
	}
	err := t.BandwidthEstimator.WriteRTCP(pkts, attrs)
	if len(fbs) > 0 {
		rec := map[string]any{"packets": fbs, "targetAfter": t.GetTargetBitrate(), "statsAfter": t.GetStats()}
		if err != nil {
			rec["error"] = err.Error()
		}
		t.log("feedback", now, rec)
	}
	return err
}

// TWCCPacket is one entry of a decoded TWCC feedback: the transport-wide
// sequence number, the status symbol (0 = not received, 1 = small delta,
// 2 = large delta, 3 = received without delta) and the receive time in ms on
// the receiver's TWCC clock (reference time + cumulative deltas; nil if lost
// or reported without a delta).
type TWCCPacket struct {
	Seq    uint16   `json:"seq"`
	Status uint16   `json:"status"`
	RecvMs *float64 `json:"recvMs"`
}

// DecodeTWCC expands a TWCC feedback packet into per-packet receive times.
func DecodeTWCC(fb *rtcp.TransportLayerCC) map[string]any {
	var symbols []uint16
	for _, c := range fb.PacketChunks {
		switch c := c.(type) {
		case *rtcp.RunLengthChunk:
			for i := 0; i < int(c.RunLength); i++ {
				symbols = append(symbols, c.PacketStatusSymbol)
			}
		case *rtcp.StatusVectorChunk:
			symbols = append(symbols, c.SymbolList...)
		}
	}
	if len(symbols) > int(fb.PacketStatusCount) {
		symbols = symbols[:fb.PacketStatusCount]
	}
	pkts := make([]TWCCPacket, 0, len(symbols))
	cumUs := int64(fb.ReferenceTime) * 64000 // reference time is in 64 ms units
	di := 0
	received := 0
	for i, sym := range symbols {
		p := TWCCPacket{Seq: fb.BaseSequenceNumber + uint16(i), Status: sym}
		if sym == rtcp.TypeTCCPacketReceivedSmallDelta || sym == rtcp.TypeTCCPacketReceivedLargeDelta {
			if di < len(fb.RecvDeltas) {
				cumUs += fb.RecvDeltas[di].Delta // pion stores deltas in microseconds
				di++
				ms := float64(cumUs) / 1000
				p.RecvMs = &ms
			}
			received++
		} else if sym == rtcp.TypeTCCPacketReceivedWithoutDelta {
			received++
		}
		pkts = append(pkts, p)
	}
	rec := map[string]any{
		"type": "twcc", "senderSsrc": fb.SenderSSRC, "mediaSsrc": fb.MediaSSRC,
		"baseSeq": fb.BaseSequenceNumber, "statusCount": fb.PacketStatusCount,
		"refTimeMs": float64(fb.ReferenceTime) * 64, "fbPktCount": fb.FbPktCount,
		"received": received, "lost": len(pkts) - received, "pkts": pkts,
	}
	if raw, err := fb.Marshal(); err == nil {
		rec["raw"] = base64.StdEncoding.EncodeToString(raw)
	}
	return rec
}

func decodeCCFB(fb *rtcp.CCFeedbackReport) map[string]any {
	rec := map[string]any{"type": "ccfb", "senderSsrc": fb.SenderSSRC, "reportTimestamp": fb.ReportTimestamp, "blocks": fb.ReportBlocks}
	if raw, err := fb.Marshal(); err == nil {
		rec["raw"] = base64.StdEncoding.EncodeToString(raw)
	}
	return rec
}
