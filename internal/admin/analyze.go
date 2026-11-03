package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Point is [seconds since session creation, value].
type Point [2]float64

// Marker is a discrete event drawn on the charts' time axis.
type Marker struct {
	T    float64 `json:"t"`
	Src  string  `json:"src"` // server, client, rtcp, encoder, cc
	Kind string  `json:"kind"`
	Text string  `json:"text"`
}

// Series is the payload of GET .../series.
type Series struct {
	CreatedMs     float64            `json:"createdMs"`
	DurationSec   float64            `json:"durationSec"`
	BucketSec     float64            `json:"bucketSec"`
	ClockOffsetMs float64            `json:"clockOffsetMs"` // server clock − browser clock
	Series        map[string][]Point `json:"series"`
	Markers       []Marker           `json:"markers"`
	MarkersTotal  int                `json:"markersTotal"`
}

type analysis struct {
	summary map[string]any
	series  *Series
	frames  []FrameRow // one per encoded video frame, in encode order (see frames.go)
}

// ---------------------------------------------------------------------------
// Small numeric helpers
// ---------------------------------------------------------------------------

type dist struct{ vals []float64 }

func (d *dist) add(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	d.vals = append(d.vals, v)
}

func (d *dist) MarshalJSON() ([]byte, error) {
	if len(d.vals) == 0 {
		return []byte("null"), nil
	}
	s := append([]float64(nil), d.vals...)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[int(math.Min(float64(len(s)-1), math.Floor(p*float64(len(s)))))] }
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	return json.Marshal(map[string]any{
		"n": len(s), "min": s[0], "max": s[len(s)-1], "mean": sum / float64(len(s)),
		"median": q(0.5), "p95": q(0.95), "p99": q(0.99),
	})
}

// bucket aggregates values into fixed-width time bins.
type agg struct {
	n             int
	sum, min, max float64
}

type bucketer struct {
	width float64
	bins  map[int]*agg
}

func newBucketer(width float64) *bucketer { return &bucketer{width: width, bins: map[int]*agg{}} }

func (b *bucketer) add(t, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	i := int(math.Floor(t / b.width))
	a := b.bins[i]
	if a == nil {
		a = &agg{min: v, max: v}
		b.bins[i] = a
	}
	a.n++
	a.sum += v
	a.min = math.Min(a.min, v)
	a.max = math.Max(a.max, v)
}

func (b *bucketer) count(t float64) { b.add(t, 1) }

func (b *bucketer) points(f func(a *agg) float64) []Point {
	keys := make([]int, 0, len(b.bins))
	for k := range b.bins {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		out = append(out, Point{(float64(k) + 0.5) * b.width, f(b.bins[k])})
	}
	return out
}

func mean(a *agg) float64  { return a.sum / float64(a.n) }
func maxOf(a *agg) float64 { return a.max }
func minOf(a *agg) float64 { return a.min }
func sum(a *agg) float64   { return a.sum }

// sumPerSec turns a per-bucket total into a rate.
func (b *bucketer) sumPerSec(scale float64) []Point {
	return b.points(func(a *agg) float64 { return a.sum * scale / b.width })
}

func niceBucket(durSec float64) float64 {
	want := durSec / 1200
	for _, w := range []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60} {
		if want <= w {
			return w
		}
	}
	return 120
}

// ---------------------------------------------------------------------------
// The analysis
// ---------------------------------------------------------------------------

func analyze(dir string, info SessionInfo) (*analysis, error) {
	t0 := info.CreatedMs
	rel := func(t float64) float64 { return (t - t0) / 1000 }
	dur := info.DurationSec
	if dur <= 0 {
		dur = 60
	}
	bw := niceBucket(dur)
	ser := &Series{CreatedMs: t0, BucketSec: bw, Series: map[string][]Point{}}
	ft := newFrameTracker() // per-frame latency join across encoder, cc and client
	S := ser.Series
	var markers []Marker
	markerCounts := map[string]int{}
	const markerCap = 400 // per (src, kind)
	mark := func(t float64, src, kind, text string) {
		ser.MarkersTotal++
		key := src + "/" + kind
		markerCounts[key]++
		if markerCounts[key] <= markerCap {
			markers = append(markers, Marker{T: rel(t), Src: src, Kind: kind, Text: text})
		}
	}
	files := map[string]map[string]any{}
	fileInfo := func(name string) map[string]any {
		f := files[name]
		if f == nil {
			f = map[string]any{"name": name + ".jsonl", "lines": 0, "kinds": map[string]int{}, "bytes": info.Files[name+".jsonl"]}
			files[name] = f
		}
		return f
	}
	count := func(name string, r *record) {
		f := fileInfo(name)
		f["lines"] = f["lines"].(int) + 1
		f["kinds"].(map[string]int)[r.Kind]++
	}
	var maxT float64
	seeT := func(t float64) {
		if t > maxT {
			maxT = t
		}
	}

	// -- events.jsonl -------------------------------------------------------
	serverEvents := []map[string]any{}
	var endReason string
	err := scan(dir, "events", func(_ []byte, r *record) bool {
		count("events", r)
		seeT(r.T)
		var d struct {
			Value string `json:"value"`
		}
		decode(r.Data, &d)
		serverEvents = append(serverEvents, map[string]any{"t": rel(r.T), "kind": r.Kind, "value": d.Value})
		mark(r.T, "server", r.Kind, d.Value)
		if r.Kind == "closed" {
			endReason = d.Value
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	// -- snapshots.jsonl ----------------------------------------------------
	type snapData struct {
		CC struct {
			TargetBitrate      float64 `json:"targetBitrate"`
			LossTargetBitrate  float64 `json:"lossTargetBitrate"`
			DelayTargetBitrate float64 `json:"delayTargetBitrate"`
			DelayEstimate      float64 `json:"delayEstimate"`
			DelayMeasurement   float64 `json:"delayMeasurement"`
			DelayThreshold     float64 `json:"delayThreshold"`
			AverageLoss        float64 `json:"averageLoss"`
			State              string  `json:"state"`
			Usage              string  `json:"usage"`
		} `json:"cc"`
		Encoder struct {
			ConfiguredBitrate float64 `json:"configuredBitrate"`
			OutputBitrate     float64 `json:"outputBitrate"`
			OutputFps         float64 `json:"outputFps"`
			FramesSkipped     float64 `json:"framesSkipped"`
		} `json:"encoder"`
		RTCP map[string]struct {
			RttMs float64 `json:"rttMs"`
		} `json:"rtcp"`
		RTP map[string]struct {
			FractionLost float64 `json:"fractionLost"`
			JitterMs     float64 `json:"jitterMs"`
		} `json:"rtp"`
		DCBuffered float64 `json:"dataChannelBufferedBytes"`
		Media      struct {
			PositionSec float64 `json:"positionSec"`
			Loops       float64 `json:"loops"`
		} `json:"media"`
		Simulcast *struct { // simulcast sessions only
			Layer        string  `json:"layer"`
			LayerIndex   float64 `json:"layerIndex"`
			LayerBps     float64 `json:"layerBps"`
			BudgetBps    float64 `json:"budgetBps"`
			PaddingBps   float64 `json:"paddingBps"`
			SentTotalBps float64 `json:"sentTotalBps"`
			SentVideoBps float64 `json:"sentVideoBps"`
		} `json:"simulcast"`
	}
	snapshots := 0
	var lastSnap map[string]any
	var lastState, lastUsage string
	var lastLoops float64
	var lastSnapLine []byte
	err = scan(dir, "snapshots", func(line []byte, r *record) bool {
		count("snapshots", r)
		seeT(r.T)
		snapshots++
		lastSnapLine = append(lastSnapLine[:0], line...)
		var d snapData
		if !decode(r.Data, &d) {
			return true
		}
		t := rel(r.T)
		add := func(name string, v float64) { S[name] = append(S[name], Point{t, v}) }
		add("ccTargetKbps", d.CC.TargetBitrate/1000)
		add("lossTargetKbps", d.CC.LossTargetBitrate/1000)
		add("delayTargetKbps", d.CC.DelayTargetBitrate/1000)
		add("gccDelayEstimateMs", d.CC.DelayEstimate)
		add("gccDelayMeasurementMs", d.CC.DelayMeasurement)
		add("gccDelayThresholdMs", d.CC.DelayThreshold)
		add("gccAverageLossPct", d.CC.AverageLoss*100)
		add("encCfgKbps", d.Encoder.ConfiguredBitrate/1000)
		add("encOutKbps", d.Encoder.OutputBitrate/1000)
		add("encFps", d.Encoder.OutputFps)
		add("dcBufferedBytes", d.DCBuffered)
		if v, ok := d.RTCP["video"]; ok && v.RttMs > 0 {
			add("rrRttVideoMs", v.RttMs)
		}
		if v, ok := d.RTP["video"]; ok {
			add("rrFractionLostVideoPct", v.FractionLost*100)
			add("rrJitterVideoMs", v.JitterMs)
		}
		if a, ok := d.RTP["audio"]; ok {
			add("rrJitterAudioMs", a.JitterMs)
		}
		if d.CC.State != lastState || d.CC.Usage != lastUsage {
			if lastState != "" {
				mark(r.T, "cc", "state", fmt.Sprintf("GCC %s / %s", d.CC.State, d.CC.Usage))
			}
			lastState, lastUsage = d.CC.State, d.CC.Usage
		}
		if d.Media.Loops != lastLoops {
			mark(r.T, "server", "loop", fmt.Sprintf("source looped (%v)", d.Media.Loops))
			lastLoops = d.Media.Loops
		}
		if sim := d.Simulcast; sim != nil {
			add("simBudgetKbps", sim.BudgetBps/1000)
			add("simLayerKbps", sim.LayerBps/1000)
			add("simLayerIndex", sim.LayerIndex)
			add("padKbps", sim.PaddingBps/1000)
			add("sendTotalKbps", sim.SentTotalBps/1000)
			add("sendVideoPayloadKbps", sim.SentVideoBps/1000)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if lastSnapLine != nil {
		var r record
		if decode(lastSnapLine, &r) {
			_ = json.Unmarshal(r.Data, &lastSnap)
		}
	}

	// -- encoder.jsonl ------------------------------------------------------
	type frameData struct {
		N        int     `json:"n"`
		Bytes    float64 `json:"bytes"`
		Key      bool    `json:"key"`
		Forced   bool    `json:"forced"`
		EncMs    float64 `json:"encMs"`
		WriteMs  float64 `json:"writeMs"`
		CfgBps   float64 `json:"cfgBps"`
		CCTarget float64 `json:"ccTarget"`
		RTPTs    *uint32 `json:"rtpTs"`
		Layer    string  `json:"layer"` // simulcast sessions: the rendition this frame came from
		HubMs    float64 `json:"hubMs"` // simulcast sessions: hub arrival → forwarded
	}
	enc := map[string]any{}
	framesByLayer := map[string]int{}
	hubMs := &dist{}
	var encFrames, encKeys, encForced, encSkipped, encReconf int
	var encBytes float64
	var encFirst, encLast float64
	encMs, writeMs, frameBytes := &dist{}, &dist{}, &dist{}
	bFrameBytes, bEncMs, bWriteMs, bSkipped := newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw)
	var lastFrameN int
	err = scan(dir, "encoder", func(_ []byte, r *record) bool {
		count("encoder", r)
		seeT(r.T)
		t := rel(r.T)
		switch r.Kind {
		case "frame":
			var d frameData
			if !decode(r.Data, &d) {
				return true
			}
			if encFrames == 0 {
				encFirst = r.T
			}
			encLast = r.T
			encFrames++
			lastFrameN = d.N
			encBytes += d.Bytes
			encMs.add(d.EncMs)
			writeMs.add(d.WriteMs)
			frameBytes.add(d.Bytes)
			bFrameBytes.add(t, d.Bytes)
			bEncMs.add(t, d.EncMs)
			bWriteMs.add(t, d.WriteMs)
			if d.RTPTs != nil {
				ft.addEncoded(r.T, *d.RTPTs, d.N, d.Key, d.Bytes, d.CCTarget, d.EncMs, d.WriteMs)
			}
			if d.Layer != "" {
				framesByLayer[d.Layer]++
				hubMs.add(d.HubMs)
			}
			if d.Key {
				encKeys++
				if d.Forced {
					encForced++
				}
				why := "periodic"
				if d.Forced {
					why = "forced by PLI/FIR"
				}
				mark(r.T, "encoder", "keyframe", fmt.Sprintf("IDR frame %d, %.0f kB, %s", d.N, d.Bytes/1000, why))
			}
		case "reconfig":
			encReconf++
			var d struct {
				CCTarget    float64 `json:"ccTarget"`
				VideoTarget float64 `json:"videoTarget"`
				Applied     float64 `json:"applied"`
				Initial     bool    `json:"initial"`
				Layer       string  `json:"layer"`
			}
			decode(r.Data, &d)
			switch {
			case d.Layer != "" && d.Initial:
				mark(r.T, "encoder", "reconfig", fmt.Sprintf("simulcast: waiting for layer %s (%.0f kbps, CC %.0f)", d.Layer, d.Applied/1000, d.CCTarget/1000))
			case d.Layer != "":
				mark(r.T, "encoder", "layer", fmt.Sprintf("forwarding layer %s (%.0f kbps, budget %.0f, CC %.0f)", d.Layer, d.Applied/1000, d.VideoTarget/1000, d.CCTarget/1000))
			case d.Initial:
				mark(r.T, "encoder", "reconfig", fmt.Sprintf("encoder started at %.0f kbps (CC %.0f)", d.Applied/1000, d.CCTarget/1000))
			}
		case "skipped":
			encSkipped++
			ft.skipped++
			bSkipped.count(t)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	S["encFrameBytesMean"] = bFrameBytes.points(mean)
	S["encFrameBytesMax"] = bFrameBytes.points(maxOf)
	S["encFrameKbps"] = bFrameBytes.sumPerSec(8.0 / 1000)
	S["encMsMean"] = bEncMs.points(mean)
	S["encMsMax"] = bEncMs.points(maxOf)
	S["writeMsMax"] = bWriteMs.points(maxOf)
	S["encSkipped"] = bSkipped.points(sum)
	encDur := (encLast - encFirst) / 1000
	enc["frames"], enc["lastFrameN"], enc["bytes"], enc["keyframes"], enc["keyframesForced"], enc["skipped"], enc["reconfigs"] = encFrames, lastFrameN, encBytes, encKeys, encForced, encSkipped, encReconf
	enc["encMs"], enc["writeMs"], enc["frameBytes"] = encMs, writeMs, frameBytes
	if len(framesByLayer) > 0 {
		enc["source"] = "simulcast"
		enc["framesByLayer"], enc["hubMs"] = framesByLayer, hubMs
	}
	if encDur > 0 {
		enc["avgBitrateKbps"] = encBytes * 8 / 1000 / encDur
		enc["avgFps"] = float64(encFrames) / encDur
		enc["firstFrameAt"], enc["lastFrameAt"] = rel(encFirst), rel(encLast)
	}

	// -- cc.jsonl -----------------------------------------------------------
	type sentData struct {
		SSRC  uint32  `json:"ssrc"`
		TWCC  int     `json:"twcc"`
		Ts    uint32  `json:"ts"`
		Hdr   float64 `json:"hdr"`
		Len   float64 `json:"len"`
		Rtx   bool    `json:"rtx"`
		Pad   bool    `json:"pad"`
		Probe bool    `json:"probe"` // simulcast padding probe: counted by GCC, padding on the wire
		QMs   float64 `json:"qMs"`
	}
	type fbData struct {
		Packets []struct {
			Type     string `json:"type"`
			Lost     int    `json:"lost"`
			Received int    `json:"received"`
			Pkts     []struct {
				Seq    int     `json:"seq"`
				Status int     `json:"status"`
				RecvMs float64 `json:"recvMs"`
			} `json:"pkts"`
		} `json:"packets"`
		TargetAfter float64 `json:"targetAfter"`
	}
	cc := map[string]any{}
	var ccSent, ccRtx, ccPad, ccProbe, ccFb, ccFbLost, ccFbRecv, ccTargets int
	var ccBytes, ccRtxBytes, ccProbeBytes float64
	qMs := &dist{}
	sendT := map[int]float64{} // twcc seq -> send time (ms), for the delay join
	bSend, bSendVideo, bRtx, bQMs, bFbLost, bDelay := newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw)
	bProbe := newBucketer(bw)
	delayMin := math.Inf(1)
	var tMin, tMax, tLast, tPrevT, tPrev, tWeighted float64
	var ccConfig, ccStreams any
	var streams []any
	videoSSRC := uint32(0)
	var meta map[string]any
	if b, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		_ = json.Unmarshal(b, &meta)
		if f, ok := meta["videoSsrc"].(float64); ok {
			videoSSRC = uint32(f)
		}
	}
	err = scan(dir, "cc", func(_ []byte, r *record) bool {
		count("cc", r)
		seeT(r.T)
		t := rel(r.T)
		switch r.Kind {
		case "sent":
			var d sentData
			if !decode(r.Data, &d) {
				return true
			}
			ccSent++
			size := d.Len + d.Hdr
			ccBytes += size
			bSend.add(t, size)
			if d.SSRC == videoSSRC {
				bSendVideo.add(t, size)
				ft.onSent(r.T, d.Ts, d.TWCC, d.Rtx, d.QMs)
			}
			if d.Rtx {
				ccRtx++
				ccRtxBytes += size
				bRtx.count(t)
			}
			if d.Pad {
				ccPad++
			}
			if d.Probe {
				ccProbe++
				ccProbeBytes += size
				bProbe.add(t, size)
			}
			qMs.add(d.QMs)
			bQMs.add(t, d.QMs)
			if !d.Rtx {
				sendT[d.TWCC&0xffff] = r.T
			}
		case "feedback":
			var d fbData
			if !decode(r.Data, &d) {
				return true
			}
			ccFb++
			for _, p := range d.Packets {
				ccFbLost += p.Lost
				ccFbRecv += p.Received
				for _, pk := range p.Pkts {
					ft.onFeedback(pk.Seq, pk.Status, pk.RecvMs)
					st, ok := sendT[pk.Seq&0xffff]
					if !ok {
						continue
					}
					delete(sendT, pk.Seq&0xffff)
					if pk.Status == 0 {
						bFbLost.count(rel(st))
						continue
					}
					delta := pk.RecvMs - st
					if delta < delayMin {
						delayMin = delta
					}
					bDelay.add(rel(st), delta)
				}
			}
		case "target":
			var d struct {
				Bitrate float64 `json:"bitrate"`
			}
			if !decode(r.Data, &d) {
				return true
			}
			ccTargets++
			S["ccTargetChangeKbps"] = append(S["ccTargetChangeKbps"], Point{t, d.Bitrate / 1000})
			if ccTargets == 1 {
				tMin, tMax = d.Bitrate, d.Bitrate
			} else {
				tWeighted += tPrev * (r.T - tPrevT)
			}
			tMin, tMax = math.Min(tMin, d.Bitrate), math.Max(tMax, d.Bitrate)
			tPrev, tPrevT, tLast = d.Bitrate, r.T, d.Bitrate
		case "config":
			_ = json.Unmarshal(r.Data, &ccConfig)
		case "stream":
			var v any
			_ = json.Unmarshal(r.Data, &v)
			streams = append(streams, v)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	ccStreams = streams
	S["sendKbps"] = bSend.sumPerSec(8.0 / 1000)
	S["sendVideoKbps"] = bSendVideo.sumPerSec(8.0 / 1000)
	S["rtxPackets"] = bRtx.points(sum)
	S["pacerQueueMsMean"] = bQMs.points(mean)
	S["pacerQueueMsMax"] = bQMs.points(maxOf)
	S["twccLostPackets"] = bFbLost.points(sum)
	if !math.IsInf(delayMin, 1) {
		S["twccDelayMsMean"] = bDelay.points(func(a *agg) float64 { return mean(a) - delayMin })
		S["twccDelayMsMax"] = bDelay.points(func(a *agg) float64 { return a.max - delayMin })
		S["twccDelayMsMin"] = bDelay.points(func(a *agg) float64 { return a.min - delayMin })
	}
	if ccTargets > 0 && maxT > tPrevT {
		tWeighted += tPrev * (maxT - tPrevT)
	}
	cc["sentPackets"], cc["sentBytes"], cc["rtxPackets"], cc["rtxBytes"], cc["paddingPackets"] = ccSent, ccBytes, ccRtx, ccRtxBytes, ccPad
	if ccProbe > 0 {
		cc["probePackets"], cc["probeBytes"] = ccProbe, ccProbeBytes
		S["probeKbps"] = bProbe.sumPerSec(8.0 / 1000)
	}
	cc["feedbacks"], cc["feedbackLost"], cc["feedbackReceived"], cc["pacerQueueMs"] = ccFb, ccFbLost, ccFbRecv, qMs
	tgt := map[string]any{"changes": ccTargets, "minBps": tMin, "maxBps": tMax, "lastBps": tLast}
	if ccTargets > 0 && maxT > t0 {
		tgt["timeWeightedMeanBps"] = tWeighted / (maxT - t0)
	}
	cc["target"], cc["config"], cc["streams"] = tgt, ccConfig, ccStreams
	if ccFbLost+ccFbRecv > 0 {
		cc["feedbackLossPct"] = 100 * float64(ccFbLost) / float64(ccFbLost+ccFbRecv)
	}

	// -- rtcp.jsonl ---------------------------------------------------------
	rtcpCounts := map[string]int{}
	nacks := []map[string]any{}
	var nackedSeqs, retransmits int
	rr := map[string]map[string]*dist{}
	rrLast := map[string]any{}
	bNackSeqs, bRetrans := newBucketer(bw), newBucketer(bw)
	err = scan(dir, "rtcp", func(_ []byte, r *record) bool {
		count("rtcp", r)
		seeT(r.T)
		rtcpCounts[r.Kind]++
		t := rel(r.T)
		switch r.Kind {
		case "nack":
			var d struct {
				ViaKind string `json:"viaKind"`
				Seqs    []int  `json:"seqs"`
			}
			decode(r.Data, &d)
			nackedSeqs += len(d.Seqs)
			bNackSeqs.add(t, float64(len(d.Seqs)))
			if len(nacks) < 200 {
				nacks = append(nacks, map[string]any{"t": t, "media": d.ViaKind, "seqs": d.Seqs})
			}
			mark(r.T, "rtcp", "nack", fmt.Sprintf("NACK %s: %d seq%s %s", d.ViaKind, len(d.Seqs), plural(len(d.Seqs)), seqList(d.Seqs)))
		case "pli", "fir":
			var d struct {
				ViaKind string `json:"viaKind"`
			}
			decode(r.Data, &d)
			mark(r.T, "rtcp", r.Kind, strings.ToUpper(r.Kind)+" from browser ("+d.ViaKind+")")
		case "remb":
			var d struct {
				Bitrate float64 `json:"bitrate"`
			}
			decode(r.Data, &d)
			mark(r.T, "rtcp", "remb", fmt.Sprintf("REMB %.0f kbps", d.Bitrate/1000))
		case "bye":
			mark(r.T, "rtcp", "bye", "RTCP BYE from browser")
		case "retransmit":
			retransmits++
			bRetrans.count(t)
		case "rr":
			var d struct {
				Reports []struct {
					Local        string  `json:"local"`
					RttMs        float64 `json:"rttMs"`
					JitterMs     float64 `json:"jitterMs"`
					FractionLost float64 `json:"fractionLost"`
					TotalLost    float64 `json:"totalLost"`
				} `json:"reports"`
			}
			decode(r.Data, &d)
			for _, rep := range d.Reports {
				m := rr[rep.Local]
				if m == nil {
					m = map[string]*dist{"rttMs": {}, "jitterMs": {}, "fractionLostPct": {}}
					rr[rep.Local] = m
				}
				if rep.RttMs > 0 {
					m["rttMs"].add(rep.RttMs)
				}
				m["jitterMs"].add(rep.JitterMs)
				m["fractionLostPct"].add(rep.FractionLost * 100)
				rrLast[rep.Local] = map[string]any{"t": t, "totalLost": rep.TotalLost, "fractionLost": rep.FractionLost, "rttMs": rep.RttMs, "jitterMs": rep.JitterMs}
				if rep.Local == "audio" && rep.RttMs > 0 {
					S["rrRttAudioMs"] = append(S["rrRttAudioMs"], Point{t, rep.RttMs})
				}
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	S["nackedSeqs"] = bNackSeqs.points(sum)
	S["retransmits"] = bRetrans.points(sum)

	// -- owd.jsonl (server side) --------------------------------------------
	var owdStart any
	serverPings := 0
	err = scan(dir, "owd", func(_ []byte, r *record) bool {
		count("owd", r)
		seeT(r.T)
		if r.Kind == "start" {
			_ = json.Unmarshal(r.Data, &owdStart)
		} else if r.Kind == "ping" {
			serverPings++
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	// -- client.jsonl -------------------------------------------------------
	type derived struct {
		RecvBitrateKbps       *float64 `json:"recvBitrateKbps"`
		AvailableIncomingKbps *float64 `json:"availableIncomingKbps"`
		AudioBitrateKbps      *float64 `json:"audioBitrateKbps"`
		FpsDecoded            *float64 `json:"fpsDecoded"`
		FpsReceived           *float64 `json:"fpsReceived"`
		RenderFps             *float64 `json:"renderFps"`
		LossPct               *float64 `json:"lossPct"`
		JitterMs              *float64 `json:"jitterMs"`
		JitterBufferDelayMs   *float64 `json:"jitterBufferDelayMs"`
		JitterBufferTargetMs  *float64 `json:"jitterBufferTargetMs"`
		JitterBufferMinMs     *float64 `json:"jitterBufferMinMs"`
		DecodeMs              *float64 `json:"decodeMs"`
		ProcessingMs          *float64 `json:"processingMs"`
		AssemblyMs            *float64 `json:"assemblyMs"`
		InterFrameDelayMs     *float64 `json:"interFrameDelayMs"`
		QP                    *float64 `json:"qp"`
		RttMs                 *float64 `json:"rttMs"`
		ConcealedPct          *float64 `json:"concealedPct"`
		AudioJitterBufferMs   *float64 `json:"audioJitterBufferMs"`
		Freezes               *float64 `json:"freezes"`
		FreezeMs              *float64 `json:"freezeMs"`
		Pauses                *float64 `json:"pauses"`
		Dropped               *float64 `json:"dropped"`
		Nacks                 *float64 `json:"nacks"`
		Plis                  *float64 `json:"plis"`
		RetransmittedPackets  *float64 `json:"retransmittedPackets"`
		Width                 *float64 `json:"width"`
		Height                *float64 `json:"height"`
	}
	type statsData struct {
		Tick    int     `json:"tick"`
		Derived derived `json:"derived"`
	}
	client := map[string]any{}
	clientKinds := map[string]int{}
	clientEventKinds := map[string]int{}
	clientEvents := []map[string]any{}
	var qoe, bye, clientInfo, hello any
	var offset float64 // server − browser clock, from the latest OWD sync
	var syncs int
	var lastDerived any
	var res string
	beaconsDown, pingUp, pingDown, pingRtt := &dist{}, &dist{}, &dist{}, &dist{}
	bBeacon, bPingUp, bPingDown, bPingRtt := newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw)
	bFreezes, bFreezeMs, bPauses, bDropped, bNacks, bPlis, bRtxRecv := newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw), newBucketer(bw)
	bRenderStall := newBucketer(bw)
	frames := 0
	var frameFirst, frameLast float64
	err = scan(dir, "client", func(_ []byte, r *record) bool {
		count("client", r)
		seeT(r.T)
		clientKinds[r.Kind]++
		tc := r.TClient + offset // browser record time on the server clock
		t := rel(tc)
		switch r.Kind {
		case "owdSync":
			var d struct {
				OffsetMs float64 `json:"offsetMs"`
			}
			if decode(r.Data, &d) {
				offset = d.OffsetMs
				syncs++
			}
		case "stats":
			var d statsData
			if !decode(r.Data, &d) {
				return true
			}
			_ = json.Unmarshal(r.Data, &lastDerived)
			v := d.Derived
			add := func(name string, p *float64) {
				if p != nil {
					S[name] = append(S[name], Point{t, *p})
				}
			}
			add("recvKbps", v.RecvBitrateKbps)
			add("availInKbps", v.AvailableIncomingKbps)
			add("audioKbps", v.AudioBitrateKbps)
			add("fpsDecoded", v.FpsDecoded)
			add("fpsReceived", v.FpsReceived)
			add("renderFps", v.RenderFps)
			add("lossPct", v.LossPct)
			add("jitterMs", v.JitterMs)
			add("jbDelayMs", v.JitterBufferDelayMs)
			add("jbTargetMs", v.JitterBufferTargetMs)
			add("jbMinMs", v.JitterBufferMinMs)
			add("decodeMs", v.DecodeMs)
			add("processingMs", v.ProcessingMs)
			add("assemblyMs", v.AssemblyMs)
			add("interFrameMs", v.InterFrameDelayMs)
			add("qp", v.QP)
			add("rttClientMs", v.RttMs)
			add("concealedPct", v.ConcealedPct)
			add("audioJbMs", v.AudioJitterBufferMs)
			cnt := func(b *bucketer, p *float64) {
				if p != nil && *p > 0 {
					b.add(t, *p)
				}
			}
			cnt(bFreezes, v.Freezes)
			cnt(bFreezeMs, v.FreezeMs)
			cnt(bPauses, v.Pauses)
			cnt(bDropped, v.Dropped)
			cnt(bNacks, v.Nacks)
			cnt(bPlis, v.Plis)
			cnt(bRtxRecv, v.RetransmittedPackets)
			if v.Width != nil && v.Height != nil && *v.Width > 0 {
				res = fmt.Sprintf("%.0fx%.0f", *v.Width, *v.Height)
			}
		case "event":
			var d map[string]any
			if !decode(r.Data, &d) {
				return true
			}
			k, _ := d["kind"].(string)
			// Derived events (freeze, packetLoss, ...) carry their fields in data.value.
			if v, ok := d["value"].(map[string]any); ok {
				for vk, vv := range v {
					if _, exists := d[vk]; !exists || vk == "kind" {
						d[vk] = vv
					}
				}
				delete(d, "value")
			}
			clientEventKinds[k]++
			if len(clientEvents) < 2000 {
				clientEvents = append(clientEvents, map[string]any{"t": t, "kind": k, "data": json.RawMessage(r.Data)})
			}
			mark(tc, "client", k, clientEventText(k, d))
			if k == "renderStall" {
				if ms, ok := d["gapMs"].(float64); ok {
					bRenderStall.add(t, ms)
				} else {
					bRenderStall.count(t)
				}
			}
		case "owdBeacon":
			var d struct {
				TSend float64 `json:"tSend"`
				OwdMs float64 `json:"owdMs"`
			}
			if decode(r.Data, &d) {
				beaconsDown.add(d.OwdMs)
				bBeacon.add(rel(d.TSend), d.OwdMs)
			}
		case "owdPing":
			var d struct {
				T2     float64 `json:"t2"`
				RttMs  float64 `json:"rttMs"`
				UpMs   float64 `json:"upMs"`
				DownMs float64 `json:"downMs"`
			}
			if decode(r.Data, &d) {
				pt := rel(d.T2)
				pingUp.add(d.UpMs)
				pingDown.add(d.DownMs)
				pingRtt.add(d.RttMs)
				bPingUp.add(pt, d.UpMs)
				bPingDown.add(pt, d.DownMs)
				bPingRtt.add(pt, d.RttMs)
			}
		case "frame":
			frames++
			if frames == 1 {
				frameFirst = tc
			}
			frameLast = tc
			ft.onClientFrame(r.Data, r.TClient, offset)
		case "qoe":
			_ = json.Unmarshal(r.Data, &qoe)
		case "bye":
			_ = json.Unmarshal(r.Data, &bye)
		case "clientInfo":
			_ = json.Unmarshal(r.Data, &clientInfo)
			var ci struct {
				TimeOrigin float64 `json:"timeOrigin"`
			}
			if decode(r.Data, &ci) && ci.TimeOrigin > 0 {
				ft.timeOrigin = ci.TimeOrigin
			}
		case "hello":
			_ = json.Unmarshal(r.Data, &hello)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	S["owdDownMs"] = bBeacon.points(mean)
	S["owdDownMaxMs"] = bBeacon.points(maxOf)
	S["owdDownMinMs"] = bBeacon.points(minOf)
	S["owdUpMs"] = bPingUp.points(mean)
	S["owdPingDownMs"] = bPingDown.points(mean)
	S["owdRttMs"] = bPingRtt.points(mean)
	S["clientFreezes"] = bFreezes.points(sum)
	S["clientFreezeMs"] = bFreezeMs.points(sum)
	S["clientPauses"] = bPauses.points(sum)
	S["clientDropped"] = bDropped.points(sum)
	S["clientNacks"] = bNacks.points(sum)
	S["clientPlis"] = bPlis.points(sum)
	S["clientRtxReceived"] = bRtxRecv.points(sum)
	S["renderStallMs"] = bRenderStall.points(sum)
	client["records"], client["eventKinds"], client["events"] = clientKinds, clientEventKinds, clientEvents
	client["qoe"], client["bye"], client["info"], client["hello"], client["lastStats"], client["resolution"] = qoe, bye, clientInfo, hello, lastDerived, res
	if frames > 0 {
		client["renderedFrames"] = frames
		client["firstRenderedAt"], client["lastRenderedAt"] = rel(frameFirst), rel(frameLast)
	}
	owd := map[string]any{
		"start": owdStart, "serverPings": serverPings, "beacons": beaconsDown, "pingUp": pingUp, "pingDown": pingDown, "pingRtt": pingRtt,
		"syncs": syncs, "clockOffsetMs": offset,
	}
	ser.ClockOffsetMs = offset

	// -- per-frame latency (join of encoder, cc and client on the RTP timestamp)
	fr := ft.finish(rel, bw, delayMin, offset, syncs)
	for k, v := range fr.series {
		S[k] = v
	}

	// Drop empty series so the page only offers what exists.
	for k, v := range S {
		if len(v) == 0 {
			delete(S, k)
		}
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].T < markers[j].T })
	ser.Markers = markers
	if maxT > 0 {
		ser.DurationSec = math.Max(info.DurationSec, rel(maxT))
	} else {
		ser.DurationSec = info.DurationSec
	}

	// meta.json minus the bulky SDP (available from GET .../meta).
	if meta != nil {
		meta = copyMap(meta)
		delete(meta, "offer")
		delete(meta, "answer")
		delete(meta, "files")
	}
	fileList := []any{}
	for _, name := range streamFiles {
		if _, ok := info.Files[name+".jsonl"]; ok {
			fileList = append(fileList, fileInfo(name))
		}
	}
	info.DurationSec = ser.DurationSec
	summary := map[string]any{
		"session":      info,
		"endReason":    endReason,
		"meta":         meta,
		"files":        fileList,
		"serverEvents": serverEvents,
		"snapshots":    map[string]any{"count": snapshots, "last": lastSnap},
		"encoder":      enc,
		"cc":           cc,
		"rtcp":         map[string]any{"counts": rtcpCounts, "nacks": nacks, "nackedSeqs": nackedSeqs, "retransmits": retransmits, "rr": rr, "rrLast": rrLast},
		"client":       client,
		"owd":          owd,
		"frames":       fr.summary,
		"markers":      ser.MarkersTotal,
	}
	// Simulcast sessions: the layer ladder from meta.json plus the forwarding
	// counters of the last snapshot (switches, keyframe waits, padding, startup).
	if meta != nil && meta["simulcast"] != nil {
		sim := map[string]any{"config": meta["simulcast"], "framesByLayer": framesByLayer}
		if lastSnap != nil {
			sim["last"] = lastSnap["simulcast"]
			sim["hub"] = lastSnap["media"]
		}
		summary["simulcast"] = sim
	}
	return &analysis{summary: summary, series: ser, frames: fr.rows}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func seqList(seqs []int) string {
	if len(seqs) == 0 {
		return ""
	}
	parts := make([]string, 0, 8)
	for i, s := range seqs {
		if i == 8 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprint(s))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// clientEventText renders a browser event as one line for markers and timelines.
func clientEventText(kind string, d map[string]any) string {
	num := func(k string) string {
		if v, ok := d[k].(float64); ok {
			if v == math.Trunc(v) {
				return fmt.Sprintf("%.0f", v)
			}
			return fmt.Sprintf("%.1f", v)
		}
		return "?"
	}
	str := func(k string) string {
		if v, ok := d[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	switch kind {
	case "freeze":
		return fmt.Sprintf("freeze ×%s, %s ms (total %s)", num("count"), num("durationMs"), num("totalFreezes"))
	case "pause":
		return fmt.Sprintf("pause ×%s, %s ms", num("count"), num("durationMs"))
	case "packetLoss":
		return fmt.Sprintf("%s packet loss: %s lost / %s received", str("media"), num("lost"), num("received"))
	case "retransmitsReceived":
		return fmt.Sprintf("retransmits received: %s packets", num("packets"))
	case "nackSent", "pliSent", "firSent":
		return fmt.Sprintf("%s ×%s", kind, num("count"))
	case "framesDropped":
		return fmt.Sprintf("frames dropped ×%s (total %s)", num("count"), num("total"))
	case "keyframeDecoded":
		return fmt.Sprintf("keyframe decoded (total %s)", num("total"))
	case "resolution":
		return fmt.Sprintf("resolution %s → %s", str("from"), str("to"))
	case "decoder":
		return fmt.Sprintf("decoder %s → %s", str("from"), str("to"))
	case "audioConcealment":
		return fmt.Sprintf("audio concealment ×%s (%s samples)", num("events"), num("concealedSamples"))
	case "renderStall":
		return fmt.Sprintf("render stall: %s ms between frames (threshold %s ms)", num("gapMs"), num("thresholdMs"))
	case "presentationSkipped":
		return fmt.Sprintf("compositor skipped %s frame presentations", num("skipped"))
	case "visibility":
		return fmt.Sprintf("page %v", d["value"])
	case "start", "stop":
		return fmt.Sprintf("%s: %v", kind, d["value"])
	case "firstRenderedFrame":
		return fmt.Sprintf("first rendered frame %v", d["value"])
	case "candidatePairChange":
		return fmt.Sprintf("candidate pair %s → %s", str("from"), str("to"))
	}
	if v, ok := d["value"]; ok {
		return fmt.Sprintf("%s: %v", kind, v)
	}
	parts := []string{}
	for k, v := range d {
		if k != "kind" {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return kind
	}
	return kind + " " + strings.Join(parts, " ")
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
