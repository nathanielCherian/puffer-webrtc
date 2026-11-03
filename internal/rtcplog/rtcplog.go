// Package rtcplog decodes every RTCP packet the browser sends to the server
// and logs it with its arrival time, while keeping running counters for the
// periodic snapshots.
//
// Pion delivers each RTCP packet to every local sender it addresses (an RR
// with report blocks for video and audio reaches both senders). Observe is
// called per sender and logs a packet only from the reader of its first local
// destination SSRC, so each packet appears exactly once.
package rtcplog

import (
	"encoding/base64"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// Logger receives one record; kind is the RTCP packet type.
type Logger func(kind string, t time.Time, data any)

type local struct {
	name      string
	clockRate uint32
	c         Counters
}

// Counters tallies what one local sender received.
type Counters struct {
	PLI, FIR, NackPackets, NackedSeqs, RR, SR, REMB, TWCC, XR, SDES, Bye, Other int64
	LastRR                                                                      map[string]any
	LastRTTMs                                                                   float64
	RTTSamples                                                                  int64
	LastREMB                                                                    float32
	LastPLIAt                                                                   time.Time
	LastNackAt                                                                  time.Time
}

// Observer logs RTCP for a set of local senders.
type Observer struct {
	log     Logger
	mu      sync.Mutex
	locals  map[uint32]*local
	primary uint32 // logs packets that address no local SSRC
}

// New creates an observer.
func New(log Logger) *Observer { return &Observer{log: log, locals: map[uint32]*local{}} }

// AddLocal registers a local sender SSRC. The first one becomes the primary.
func (o *Observer) AddLocal(ssrc uint32, name string, clockRate uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.locals) == 0 {
		o.primary = ssrc
	}
	o.locals[ssrc] = &local{name: name, clockRate: clockRate}
}

// Observe handles a batch delivered to the local sender via. It reports
// whether the batch asks for a keyframe (PLI/FIR) on that sender.
func (o *Observer) Observe(via uint32, pkts []rtcp.Packet) (keyframe bool) {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	l := o.locals[via]
	if l == nil {
		return false
	}
	for _, p := range pkts {
		switch p.(type) {
		case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
			keyframe = true
		}
		if !o.shouldLog(via, p) {
			continue
		}
		kind, data := o.decode(now, l, p)
		data["via"] = via
		data["viaKind"] = l.name
		if raw, err := p.Marshal(); err == nil {
			data["raw"] = base64.StdEncoding.EncodeToString(raw)
		}
		o.log(kind, now, data)
	}
	return keyframe
}

func (o *Observer) shouldLog(via uint32, p rtcp.Packet) bool {
	for _, d := range p.DestinationSSRC() {
		if _, ok := o.locals[d]; ok {
			return d == via
		}
	}
	return via == o.primary
}

func (o *Observer) decode(now time.Time, l *local, p rtcp.Packet) (string, map[string]any) {
	c := &l.c
	switch p := p.(type) {
	case *rtcp.PictureLossIndication:
		c.PLI++
		c.LastPLIAt = now
		return "pli", map[string]any{"senderSsrc": p.SenderSSRC, "mediaSsrc": p.MediaSSRC}
	case *rtcp.FullIntraRequest:
		c.FIR++
		return "fir", map[string]any{"senderSsrc": p.SenderSSRC, "mediaSsrc": p.MediaSSRC, "fir": p.FIR}
	case *rtcp.TransportLayerNack:
		c.NackPackets++
		c.LastNackAt = now
		var seqs []uint16
		pairs := make([]map[string]any, 0, len(p.Nacks))
		for _, pair := range p.Nacks {
			list := pair.PacketList()
			seqs = append(seqs, list...)
			pairs = append(pairs, map[string]any{"pid": pair.PacketID, "blp": pair.LostPackets})
		}
		c.NackedSeqs += int64(len(seqs))
		return "nack", map[string]any{"senderSsrc": p.SenderSSRC, "mediaSsrc": p.MediaSSRC, "seqs": seqs, "count": len(seqs), "pairs": pairs}
	case *rtcp.ReceiverReport:
		c.RR++
		return "rr", map[string]any{"ssrc": p.SSRC, "reports": o.reports(now, p.Reports), "profileExtensions": p.ProfileExtensions}
	case *rtcp.SenderReport:
		c.SR++
		return "sr", map[string]any{"ssrc": p.SSRC, "ntpTime": p.NTPTime, "rtpTime": p.RTPTime, "packetCount": p.PacketCount,
			"octetCount": p.OctetCount, "reports": o.reports(now, p.Reports)}
	case *rtcp.ReceiverEstimatedMaximumBitrate:
		c.REMB++
		c.LastREMB = p.Bitrate
		return "remb", map[string]any{"senderSsrc": p.SenderSSRC, "bitrate": p.Bitrate, "ssrcs": p.SSRCs}
	case *rtcp.TransportLayerCC:
		c.TWCC++
		// The decoded per-packet contents are in cc.jsonl ("feedback").
		return "twcc", map[string]any{"senderSsrc": p.SenderSSRC, "mediaSsrc": p.MediaSSRC, "baseSeq": p.BaseSequenceNumber,
			"statusCount": p.PacketStatusCount, "refTimeMs": float64(p.ReferenceTime) * 64, "fbPktCount": p.FbPktCount,
			"recvDeltas": len(p.RecvDeltas)}
	case *rtcp.ExtendedReport:
		c.XR++
		return "xr", map[string]any{"senderSsrc": p.SenderSSRC, "reports": p.Reports}
	case *rtcp.SourceDescription:
		c.SDES++
		return "sdes", map[string]any{"chunks": p.Chunks}
	case *rtcp.Goodbye:
		c.Bye++
		return "bye", map[string]any{"sources": p.Sources, "reason": p.Reason}
	default:
		c.Other++
		return "other", map[string]any{"goType": typeName(p), "packet": p}
	}
}

// reports decodes RR/SR reception report blocks, computing the RTT from
// LSR/DLSR when the block refers to one of our SRs (RFC 3550 6.4.1).
func (o *Observer) reports(now time.Time, blocks []rtcp.ReceptionReport) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, r := range blocks {
		m := map[string]any{
			"ssrc": r.SSRC, "fractionLost": float64(r.FractionLost) / 256, "fractionLostRaw": r.FractionLost,
			"totalLost": r.TotalLost, "highestSeq": r.LastSequenceNumber, "jitter": r.Jitter,
			"lsr": r.LastSenderReport, "dlsr": r.Delay,
		}
		l := o.locals[r.SSRC]
		if l != nil {
			m["local"] = l.name
			if l.clockRate > 0 {
				m["jitterMs"] = float64(r.Jitter) / float64(l.clockRate) * 1000
			}
		}
		if r.LastSenderReport != 0 {
			rtt := ntpCompact(now) - r.LastSenderReport - r.Delay
			if rtt < 1<<31 {
				ms := float64(rtt) / 65536 * 1000
				m["rttMs"] = ms
				if l != nil {
					l.c.LastRTTMs = ms
					l.c.RTTSamples++
				}
			}
		}
		if l != nil {
			l.c.LastRR = m
		}
		out = append(out, m)
	}
	return out
}

func ntpCompact(t time.Time) uint32 {
	const ntpEpochOffset = 2208988800
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return uint32((secs<<32 | frac) >> 16)
}

func typeName(p rtcp.Packet) string {
	switch p.(type) {
	case *rtcp.RawPacket:
		return "RawPacket"
	default:
		return "unknown"
	}
}

// Snapshot returns the counters per local sender name.
func (o *Observer) Snapshot() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]any{}
	for ssrc, l := range o.locals {
		c := l.c
		m := map[string]any{
			"ssrc": ssrc, "pli": c.PLI, "fir": c.FIR, "nackPackets": c.NackPackets, "nackedSeqs": c.NackedSeqs,
			"rr": c.RR, "sr": c.SR, "remb": c.REMB, "twcc": c.TWCC, "xr": c.XR, "sdes": c.SDES, "bye": c.Bye, "other": c.Other,
		}
		if c.RTTSamples > 0 {
			m["rttMs"] = c.LastRTTMs
			m["rttSamples"] = c.RTTSamples
		}
		if c.REMB > 0 {
			m["lastRembBps"] = c.LastREMB
		}
		if c.LastRR != nil {
			m["lastReport"] = c.LastRR
		}
		if !c.LastPLIAt.IsZero() {
			m["msSinceLastPli"] = float64(time.Since(c.LastPLIAt).Microseconds()) / 1000
		}
		if !c.LastNackAt.IsZero() {
			m["msSinceLastNack"] = float64(time.Since(c.LastNackAt).Microseconds()) / 1000
		}
		out[l.name] = m
	}
	return out
}
