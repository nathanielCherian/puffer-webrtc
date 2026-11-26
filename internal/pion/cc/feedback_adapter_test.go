package cc

import (
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const twccExt = 3

// send records packets with the given transport-wide sequence numbers as sent
// at t0, t0+gap, ...
func send(t *testing.T, f *FeedbackAdapter, t0 time.Time, gap time.Duration, seqs ...uint16) {
	t.Helper()
	attrs := interceptor.Attributes{TwccExtensionAttributesKey: uint8(twccExt)}
	for i, seq := range seqs {
		ext, err := (&rtp.TransportCCExtension{TransportSequence: seq}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		h := rtp.Header{Version: 2, SequenceNumber: seq}
		if err := h.SetExtension(twccExt, ext); err != nil {
			t.Fatal(err)
		}
		if err := f.OnSent(t0.Add(time.Duration(i)*gap), &h, 1000, attrs); err != nil {
			t.Fatal(err)
		}
	}
}

func deltas(ms ...int64) []*rtcp.RecvDelta {
	out := make([]*rtcp.RecvDelta, len(ms))
	for i, d := range ms {
		out[i] = &rtcp.RecvDelta{Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: d * 1000}
	}
	return out
}

// The history is a window of time: it holds a burst far larger than the 250
// packets upstream kept, and forgets packets older than the window.
func TestHistoryIsSizedByTime(t *testing.T) {
	f := NewFeedbackAdapter()
	t0 := time.Now()
	const n = 25000 // 10 s at 2500 packets per second
	seqs := make([]uint16, n)
	for i := range seqs {
		seqs[i] = uint16(i)
	}
	gap := HistoryWindow / n
	send(t, f, t0, gap, seqs...)
	if got := f.history.evictList.Len(); got != n {
		t.Fatalf("history holds %d of %d packets sent within the window", got, n)
	}
	acks, err := f.OnTransportCCFeedback(t0, &rtcp.TransportLayerCC{
		BaseSequenceNumber: 0, PacketStatusCount: 4,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 4}},
		RecvDeltas:   deltas(1, 1, 1, 1),
	})
	if err != nil || len(acks) != 4 {
		t.Fatalf("the oldest packets in the window: %d acks, err %v", len(acks), err)
	}

	// One more packet, half a window later, pushes out the first half.
	send(t, f, t0.Add(HistoryWindow+HistoryWindow/2), 0, n)
	if got := f.history.evictList.Len(); got < n/2-1 || got > n/2+2 {
		t.Fatalf("history holds %d packets, want about %d", got, n/2)
	}
	if _, ok := f.history.get(feedbackHistoryKey{sequenceNumber: 0}); ok {
		t.Fatal("a packet older than the window is still in the history")
	}
	if len(f.history.items) != f.history.evictList.Len() {
		t.Fatalf("index has %d entries, list %d", len(f.history.items), f.history.evictList.Len())
	}
}

// A packet the sender has no record of is skipped, not reported as lost, and
// the arrival times of the packets after it are unaffected.
func TestUnknownPacketsAreSkipped(t *testing.T) {
	for _, chunk := range []rtcp.PacketStatusChunk{
		&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 4},
		&rtcp.StatusVectorChunk{SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit, SymbolList: []uint16{
			rtcp.TypeTCCPacketReceivedSmallDelta, rtcp.TypeTCCPacketReceivedSmallDelta,
			rtcp.TypeTCCPacketReceivedSmallDelta, rtcp.TypeTCCPacketReceivedSmallDelta, 0, 0, 0}},
	} {
		f := NewFeedbackAdapter()
		send(t, f, time.Now(), time.Millisecond, 11, 13) // 10 and 12 are unknown
		acks, err := f.OnTransportCCFeedback(time.Now(), &rtcp.TransportLayerCC{
			BaseSequenceNumber: 10, PacketStatusCount: 4, ReferenceTime: 100,
			PacketChunks: []rtcp.PacketStatusChunk{chunk}, RecvDeltas: deltas(1, 2, 3, 4),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(acks) != 2 || acks[0].SequenceNumber != 11 || acks[1].SequenceNumber != 13 {
			t.Fatalf("%T: acks %+v, want packets 11 and 13 only", chunk, acks)
		}
		ref := time.Time{}.Add(100 * 64 * time.Millisecond)
		if !acks[0].Arrival.Equal(ref.Add(3*time.Millisecond)) || !acks[1].Arrival.Equal(ref.Add(10*time.Millisecond)) {
			t.Fatalf("%T: arrivals %v and %v after the reference time, want 3ms and 10ms",
				chunk, acks[0].Arrival.Sub(ref), acks[1].Arrival.Sub(ref))
		}
		for _, a := range acks {
			if a.Departure.IsZero() || a.Size == 0 {
				t.Fatalf("%T: empty acknowledgment %+v", chunk, a)
			}
		}
	}
}

// The symbols of a status vector chunk past PacketStatusCount are filler: the
// packets sent after the feedback's range must not be reported as lost.
func TestStatusVectorFillerIsNotLoss(t *testing.T) {
	f := NewFeedbackAdapter()
	send(t, f, time.Now(), time.Millisecond, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	acks, err := f.OnTransportCCFeedback(time.Now(), &rtcp.TransportLayerCC{
		BaseSequenceNumber: 0, PacketStatusCount: 3,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit, SymbolList: []uint16{
			rtcp.TypeTCCPacketReceivedSmallDelta, rtcp.TypeTCCPacketNotReceived, rtcp.TypeTCCPacketReceivedSmallDelta, 0, 0, 0, 0}}},
		RecvDeltas: deltas(1, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 3 {
		t.Fatalf("%d acks, want the 3 packets the feedback covers", len(acks))
	}
	if acks[0].Arrival.IsZero() || !acks[1].Arrival.IsZero() || acks[2].Arrival.IsZero() {
		t.Fatalf("want received, lost, received: %+v", acks)
	}
}
