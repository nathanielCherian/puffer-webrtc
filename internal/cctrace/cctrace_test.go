package cctrace

import (
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/pion/rtcp"
)

func TestDecodeTWCC(t *testing.T) {
	// 5 packets from seq 100: run-length chunk "3 received small delta", then a
	// status vector with [lost, received large delta].
	fb := &rtcp.TransportLayerCC{
		SenderSSRC: 1, MediaSSRC: 2, BaseSequenceNumber: 100, PacketStatusCount: 5,
		ReferenceTime: 10, FbPktCount: 7,
		PacketChunks: []rtcp.PacketStatusChunk{
			&rtcp.RunLengthChunk{Type: rtcp.TypeTCCRunLengthChunk, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 3},
			&rtcp.StatusVectorChunk{Type: rtcp.TypeTCCStatusVectorChunk, SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				SymbolList: []uint16{rtcp.TypeTCCPacketNotReceived, rtcp.TypeTCCPacketReceivedLargeDelta, 0, 0, 0, 0, 0}},
		},
		RecvDeltas: []*rtcp.RecvDelta{
			{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 1000},
			{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 250},
			{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: 0},
			{Type: rtcp.TypeTCCPacketReceivedLargeDelta, Delta: -500},
		},
	}
	rec := DecodeTWCC(fb)
	pkts := rec["pkts"].([]TWCCPacket)
	if len(pkts) != 5 {
		t.Fatalf("got %d packets, want 5", len(pkts))
	}
	if rec["received"] != 4 || rec["lost"] != 1 {
		t.Fatalf("received/lost = %v/%v", rec["received"], rec["lost"])
	}
	want := []struct {
		seq  uint16
		recv float64
		lost bool
	}{{100, 641, false}, {101, 641.25, false}, {102, 641.25, false}, {103, 0, true}, {104, 640.75, false}}
	for i, w := range want {
		p := pkts[i]
		if p.Seq != w.seq {
			t.Errorf("pkt %d seq %d want %d", i, p.Seq, w.seq)
		}
		if w.lost {
			if p.RecvMs != nil || p.Status != rtcp.TypeTCCPacketNotReceived {
				t.Errorf("pkt %d should be lost: %+v", i, p)
			}
			continue
		}
		if p.RecvMs == nil || *p.RecvMs != w.recv {
			t.Errorf("pkt %d recv %v want %v", i, p.RecvMs, w.recv)
		}
	}
	if rec["refTimeMs"] != 640.0 {
		t.Errorf("refTimeMs = %v", rec["refTimeMs"])
	}
	if _, ok := rec["raw"].(string); !ok {
		t.Errorf("raw packet missing")
	}
}

func TestLastTimestamp(t *testing.T) {
	var kinds []string
	tr := New(&fakeEstimator{}, func(kind string, _ time.Time, _ any) { kinds = append(kinds, kind) })
	if _, ok := tr.LastTimestamp(7); ok {
		t.Fatal("timestamp before any packet")
	}
	w := tr.AddStream(&interceptor.StreamInfo{SSRC: 7}, interceptor.RTPWriterFunc(func(*rtp.Header, []byte, interceptor.Attributes) (int, error) { return 0, nil }))
	for _, ts := range []uint32{100, 100, 190} {
		if _, err := w.Write(&rtp.Header{SSRC: 7, Timestamp: ts}, []byte{1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ts, ok := tr.LastTimestamp(7); !ok || ts != 190 {
		t.Fatalf("got %d,%v want 190", ts, ok)
	}
	if _, ok := tr.LastTimestamp(8); ok {
		t.Fatal("unknown ssrc reported")
	}
	if n := len(kinds); n != 4 { // stream + 3 sent
		t.Fatalf("logged %d records (%v), want 4", n, kinds)
	}
}

// fakeEstimator is a pass-through cc.BandwidthEstimator.
type fakeEstimator struct{}

func (fakeEstimator) AddStream(_ *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	return w
}
func (fakeEstimator) WriteRTCP([]rtcp.Packet, interceptor.Attributes) error { return nil }
func (fakeEstimator) GetTargetBitrate() int                                 { return 0 }
func (fakeEstimator) OnTargetBitrateChange(func(int))                       {}
func (fakeEstimator) GetStats() map[string]any                              { return nil }
func (fakeEstimator) Close() error                                          { return nil }
