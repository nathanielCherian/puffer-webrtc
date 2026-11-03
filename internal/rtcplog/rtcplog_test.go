package rtcplog

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

type rec struct {
	kind string
	data map[string]any
}

func newObserver() (*Observer, *[]rec) {
	var recs []rec
	o := New(func(kind string, _ time.Time, data any) { recs = append(recs, rec{kind, data.(map[string]any)}) })
	o.AddLocal(1000, "video", 90000)
	o.AddLocal(2000, "audio", 48000)
	return o, &recs
}

func TestNackDecodedAndKeyframeFlag(t *testing.T) {
	o, recs := newObserver()
	nack := &rtcp.TransportLayerNack{SenderSSRC: 1, MediaSSRC: 1000, Nacks: []rtcp.NackPair{{PacketID: 10, LostPackets: 0b101}}}
	pli := &rtcp.PictureLossIndication{SenderSSRC: 1, MediaSSRC: 1000}
	if !o.Observe(1000, []rtcp.Packet{nack, pli}) {
		t.Fatal("PLI should request a keyframe")
	}
	if len(*recs) != 2 || (*recs)[0].kind != "nack" || (*recs)[1].kind != "pli" {
		t.Fatalf("records: %+v", *recs)
	}
	seqs := (*recs)[0].data["seqs"].([]uint16)
	if len(seqs) != 3 || seqs[0] != 10 || seqs[1] != 11 || seqs[2] != 13 {
		t.Fatalf("seqs = %v", seqs)
	}
	snap := o.Snapshot()["video"].(map[string]any)
	if snap["nackPackets"] != int64(1) || snap["nackedSeqs"] != int64(3) || snap["pli"] != int64(1) {
		t.Fatalf("snapshot = %v", snap)
	}
}

func TestReceiverReportLoggedOnce(t *testing.T) {
	o, recs := newObserver()
	rr := &rtcp.ReceiverReport{SSRC: 1, Reports: []rtcp.ReceptionReport{{SSRC: 2000, Jitter: 480}, {SSRC: 1000, Jitter: 900}}}
	// Pion delivers the RR to both local senders; only the first destination logs it.
	o.Observe(1000, []rtcp.Packet{rr})
	o.Observe(2000, []rtcp.Packet{rr})
	if len(*recs) != 1 || (*recs)[0].kind != "rr" {
		t.Fatalf("records: %+v", *recs)
	}
	reports := (*recs)[0].data["reports"].([]map[string]any)
	if reports[0]["jitterMs"] != 10.0 || reports[1]["jitterMs"] != 10.0 {
		t.Fatalf("jitterMs = %v / %v", reports[0]["jitterMs"], reports[1]["jitterMs"])
	}
	if o.Snapshot()["video"].(map[string]any)["rr"] != int64(0) || o.Snapshot()["audio"].(map[string]any)["rr"] != int64(1) {
		t.Fatalf("rr counted on the wrong sender: %v", o.Snapshot())
	}
}
