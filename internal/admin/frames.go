package admin

import (
	"encoding/json"
	"math"
	"sort"
)

// Per-frame latency: every encoded video frame followed to the screen. The
// three logs are joined on the RTP timestamp (encoder frame.rtpTs = cc sent.ts
// = client frame.rtpTimestamp) during the single analysis pass, and the
// browser's performance.now() times are moved onto the server clock with
// clientInfo.timeOrigin plus the OWD clock offset in force at that moment.
//
// Stages, all in ms:
//
//	encode      frame.t → + encMs
//	write       → + writeMs (packets handed to the pacer)
//	pacerWait   enqueued → first packet sent
//	pacerSpan   first packet sent → last packet sent
//	network     last packet sent → rVFC receiveTime (last packet arrived)
//	jitterBuffer receiveTime → presentationTime − decode
//	decode      rVFC processingDuration
//	compositor  presentationTime → expectedDisplayTime
//	e2e         encode start → expectedDisplayTime
//
// twccVar is an independent cross-check: the last packet's TWCC receive time
// minus its send time, above the session minimum (the TWCC clock has no
// absolute origin).

type frameJoin struct {
	n        int
	ts       uint32
	key      bool
	bytes    float64
	ccTarget float64
	encStart float64 // absolute server ms
	encMs    float64
	writeMs  float64

	pkts, rtx, lost int
	firstSend       float64
	lastSend        float64
	maxQ            float64
	twccLastRecv    float64
	hasTwcc         bool

	rendered                        bool
	recvAbs, presentAbs, displayAbs float64
	hasRecv, hasPresent, hasDisplay bool
	decodeMs                        float64
	hasDecode                       bool
}

// FrameRow is one encoded frame as the page sees it. Optional stages are nil
// when the frame did not reach that stage.
type FrameRow struct {
	N        int     `json:"n"`
	T        float64 `json:"t"` // encode start, seconds since session creation
	Key      bool    `json:"key"`
	Bytes    float64 `json:"bytes"`
	CCKbps   float64 `json:"ccKbps"`
	Pkts     int     `json:"pkts"`
	Rtx      int     `json:"rtx"`
	Lost     int     `json:"lost"`
	Rendered bool    `json:"rendered"`
	Why      string  `json:"why,omitempty"`

	Encode       float64  `json:"encode"`
	Write        float64  `json:"write"`
	PacerWait    *float64 `json:"pacerWait"`
	PacerSpan    *float64 `json:"pacerSpan"`
	PacerQueue   *float64 `json:"pacerQueue"` // max per-packet pacer queue time
	Network      *float64 `json:"network"`
	TwccVar      *float64 `json:"twccVar"`
	JitterBuffer *float64 `json:"jitterBuffer"`
	Decode       *float64 `json:"decode"`
	Compositor   *float64 `json:"compositor"`
	E2E          *float64 `json:"e2e"`
}

// frameTracker collects the join while analyze scans the streams.
type frameTracker struct {
	byTS       map[uint32]*frameJoin
	list       []*frameJoin
	byTwcc     map[int]*frameJoin // 16-bit transport-wide seq → frame (non-RTX packets)
	timeOrigin float64
	skipped    int
}

func newFrameTracker() *frameTracker {
	return &frameTracker{byTS: map[uint32]*frameJoin{}, byTwcc: map[int]*frameJoin{}}
}

func (ft *frameTracker) addEncoded(t float64, ts uint32, n int, key bool, bytes, ccTarget, encMs, writeMs float64) {
	f := &frameJoin{n: n, ts: ts, key: key, bytes: bytes, ccTarget: ccTarget, encStart: t, encMs: encMs, writeMs: writeMs}
	// A repeated timestamp means the frame was never packetised (older servers
	// logged a stale value after the connection closed): keep the first owner.
	if _, dup := ft.byTS[ts]; !dup {
		ft.byTS[ts] = f
	}
	ft.list = append(ft.list, f)
}

// onSent is called for every video RTP packet leaving the pacer.
func (ft *frameTracker) onSent(t float64, ts uint32, twcc int, rtx bool, qMs float64) {
	f := ft.byTS[ts]
	if f == nil {
		return
	}
	if rtx {
		f.rtx++
		return
	}
	ft.byTwcc[twcc&0xffff] = f
	if f.pkts == 0 || t < f.firstSend {
		f.firstSend = t
	}
	if t > f.lastSend {
		f.lastSend = t
	}
	f.pkts++
	f.maxQ = math.Max(f.maxQ, qMs)
}

// onFeedback is called for every packet entry of a TWCC feedback. recvMs is
// on the browser's TWCC clock; lost packets have status 0.
func (ft *frameTracker) onFeedback(twcc int, status int, recvMs float64) {
	f := ft.byTwcc[twcc&0xffff]
	if f == nil {
		return
	}
	delete(ft.byTwcc, twcc&0xffff)
	if status == 0 {
		f.lost++
		return
	}
	if !f.hasTwcc || recvMs > f.twccLastRecv {
		f.twccLastRecv = recvMs
		f.hasTwcc = true
	}
}

type clientFrame struct {
	Now                float64  `json:"now"`
	RTPTimestamp       *float64 `json:"rtpTimestamp"`
	ReceiveTime        *float64 `json:"receiveTime"`
	PresentationTime   *float64 `json:"presentationTime"`
	ExpectedDisplay    *float64 `json:"expectedDisplayTime"`
	ProcessingDuration *float64 `json:"processingDuration"`
}

// onClientFrame joins a rendered frame (rVFC metadata). tClient is the
// browser's Date.now() when it was recorded, offset the server−browser clock
// offset in force then.
func (ft *frameTracker) onClientFrame(raw json.RawMessage, tClient, offset float64) {
	var d clientFrame
	if !decode(raw, &d) || d.RTPTimestamp == nil {
		return
	}
	f := ft.byTS[uint32(*d.RTPTimestamp)]
	if f == nil || f.rendered { // rVFC fires once per presented frame; keep the first
		return
	}
	f.rendered = true
	base := ft.timeOrigin
	if base == 0 {
		base = tClient - d.Now
	}
	base += offset
	if d.ProcessingDuration != nil {
		f.decodeMs, f.hasDecode = *d.ProcessingDuration*1000, true
	}
	if d.ReceiveTime != nil {
		f.recvAbs, f.hasRecv = base+*d.ReceiveTime, true
	}
	if d.PresentationTime != nil {
		f.presentAbs, f.hasPresent = base+*d.PresentationTime, true
	}
	if d.ExpectedDisplay != nil {
		f.displayAbs, f.hasDisplay = base+*d.ExpectedDisplay, true
	}
}

// pbucketer keeps every value per bin so it can report percentiles.
type pbucketer struct {
	width float64
	bins  map[int][]float64
}

func newPBucketer(width float64) *pbucketer {
	return &pbucketer{width: width, bins: map[int][]float64{}}
}

func (b *pbucketer) add(t, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	i := int(math.Floor(t / b.width))
	b.bins[i] = append(b.bins[i], v)
}

func (b *pbucketer) points(f func(sorted []float64) float64) []Point {
	keys := make([]int, 0, len(b.bins))
	for k := range b.bins {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		vals := b.bins[k]
		sort.Float64s(vals)
		out = append(out, Point{(float64(k) + 0.5) * b.width, f(vals)})
	}
	return out
}

func median(s []float64) float64 { return s[len(s)/2] }
func last(s []float64) float64   { return s[len(s)-1] }

type frameResult struct {
	summary map[string]any
	series  map[string][]Point
	rows    []FrameRow
}

// finish computes the stages, distributions, bucketed series and the per-frame
// rows. rel converts absolute ms to seconds since session creation; delayMin
// is the session minimum of (TWCC recv − send) over all video packets.
func (ft *frameTracker) finish(rel func(float64) float64, bw float64, delayMin float64, offsetMs float64, syncs int) *frameResult {
	res := &frameResult{series: map[string][]Point{}}
	if len(ft.list) == 0 {
		res.summary = map[string]any{"available": false, "reason": "no encoder frame records with rtpTs (logged by servers built after per-frame RTP timestamps were added)"}
		return res
	}
	// Why was a frame not rendered? Needs the first/last rendered frame.
	firstR, lastR := -1, -1
	for _, f := range ft.list {
		if f.rendered {
			if firstR < 0 {
				firstR = f.n
			}
			lastR = f.n
		}
	}
	type stage struct {
		key  string
		dist *dist
		b    *pbucketer
	}
	mk := func(key string) *stage { return &stage{key: key, dist: &dist{}, b: newPBucketer(bw)} }
	stEnc, stWrite, stWait, stSpan, stNet, stTwcc, stJB, stDec, stComp, stE2E := mk("encode"), mk("write"), mk("pacerWait"), mk("pacerSpan"), mk("network"), mk("twccVar"), mk("jitterBuffer"), mk("decode"), mk("compositor"), mk("e2e")
	stages := []*stage{stEnc, stWrite, stWait, stSpan, stNet, stTwcc, stJB, stDec, stComp, stE2E}
	keyE2E := &dist{}
	bEncoded, bRendered, bNotRendered, bLost := newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw)
	whys := map[string]int{}
	rendered := 0
	rows := make([]FrameRow, 0, len(ft.list))
	opt := func(st *stage, t, v float64) *float64 {
		st.dist.add(v)
		st.b.add(t, v)
		return &v
	}
	for _, f := range ft.list {
		t := rel(f.encStart)
		row := FrameRow{N: f.n, T: t, Key: f.key, Bytes: f.bytes, CCKbps: f.ccTarget / 1000, Pkts: f.pkts, Rtx: f.rtx, Lost: f.lost, Rendered: f.rendered,
			Encode: f.encMs, Write: f.writeMs}
		stEnc.dist.add(f.encMs)
		stEnc.b.add(t, f.encMs)
		stWrite.dist.add(f.writeMs)
		stWrite.b.add(t, f.writeMs)
		bEncoded.count(t)
		if f.lost > 0 {
			bLost.add(t, float64(f.lost))
		}
		enqueued := f.encStart + f.encMs + f.writeMs
		if f.pkts > 0 {
			row.PacerWait = opt(stWait, t, f.firstSend-enqueued)
			row.PacerSpan = opt(stSpan, t, f.lastSend-f.firstSend)
			q := f.maxQ
			row.PacerQueue = &q
			if f.hasTwcc && !math.IsInf(delayMin, 1) {
				row.TwccVar = opt(stTwcc, t, (f.twccLastRecv-f.lastSend)-delayMin)
			}
		}
		if f.rendered {
			rendered++
			bRendered.count(t)
			if f.hasDecode {
				row.Decode = opt(stDec, t, f.decodeMs)
			}
			if f.hasRecv && f.pkts > 0 {
				row.Network = opt(stNet, t, f.recvAbs-f.lastSend)
			}
			if f.hasRecv && f.hasPresent {
				row.JitterBuffer = opt(stJB, t, (f.presentAbs-f.recvAbs)-f.decodeMs)
			}
			if f.hasPresent && f.hasDisplay {
				row.Compositor = opt(stComp, t, f.displayAbs-f.presentAbs)
			}
			if f.hasDisplay {
				row.E2E = opt(stE2E, t, f.displayAbs-f.encStart)
				if f.key {
					keyE2E.add(*row.E2E)
				}
			}
		} else {
			bNotRendered.count(t)
			switch {
			case f.pkts == 0:
				row.Why = "noPackets"
			case f.lost > f.rtx:
				row.Why = "packetsLost"
			case firstR >= 0 && f.n < firstR:
				row.Why = "beforeFirstRender"
			case lastR >= 0 && f.n > lastR:
				row.Why = "afterLastRender"
			default:
				row.Why = "notPresented"
			}
			whys[row.Why]++
		}
		rows = append(rows, row)
	}
	res.rows = rows

	S := res.series
	for _, st := range stages {
		if len(st.dist.vals) == 0 {
			continue
		}
		S["fr"+upperFirst(st.key)+"Ms"] = st.b.points(median)
	}
	S["frE2eMaxMs"] = stE2E.b.points(last)
	S["frNetworkMaxMs"] = stNet.b.points(last)
	S["frJitterBufferMaxMs"] = stJB.b.points(last)
	S["frPacerSpanMaxMs"] = stSpan.b.points(last)
	S["frEncoded"] = bEncoded.points(sum)
	S["frRendered"] = bRendered.points(sum)
	S["frNotRendered"] = bNotRendered.points(sum)
	S["frLostPackets"] = bLost.points(sum)
	for k, v := range S {
		if len(v) == 0 {
			delete(S, k)
		}
	}

	// The worst frames by end-to-end latency, for the overview.
	idx := make([]int, 0, len(rows))
	for i := range rows {
		if rows[i].E2E != nil {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return *rows[idx[a]].E2E > *rows[idx[b]].E2E })
	worst := make([]FrameRow, 0, 30)
	for i := 0; i < len(idx) && i < 30; i++ {
		worst = append(worst, rows[idx[i]])
	}
	stageMap := map[string]any{}
	for _, st := range stages {
		stageMap[st.key] = st.dist
	}
	n := len(ft.list)
	res.summary = map[string]any{
		"available":   true,
		"encoded":     n,
		"skipped":     ft.skipped,
		"rendered":    rendered,
		"renderedPct": 100 * float64(rendered) / float64(n),
		"notRendered": whys,
		"stages":      stageMap,
		"keyframes":   map[string]any{"rendered": len(keyE2E.vals), "e2e": keyE2E},
		"worst":       worst,
		"clock":       map[string]any{"timeOrigin": ft.timeOrigin != 0, "offsetMs": offsetMs, "syncs": syncs},
	}
	return res
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
